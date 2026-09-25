package media

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

// An independent codec of the ISO base media file format (ISO/IEC
// 14496-12) and of the iTunes metadata list, for the M4A fixtures and for a
// second reading of what the helper writes (NOTES.md N-170). It shares no
// code with the helper (native/musiclib-tags/src/mp4.cpp, m4a.cpp): it is
// written from the specification, and builds what taggers write around the
// audio the pinned FFmpeg encodes.

// mp4Node is a box: its type and its payload.
type mp4Node struct {
	typ     string
	payload []byte
	// raw is the box as read, header included (nil for a built one).
	raw []byte
}

// mp4Box renders a box with a 32-bit size around the concatenated parts.
func mp4Box(typ string, parts ...[]byte) []byte {
	payload := slices.Concat(parts...)
	return slices.Concat(binary.BigEndian.AppendUint32(nil, uint32(8+len(payload))), []byte(typ), payload)
}

// mp4Box64 renders a box with a 64-bit size.
func mp4Box64(typ string, parts ...[]byte) []byte {
	payload := slices.Concat(parts...)
	return slices.Concat(be32(1), []byte(typ), binary.BigEndian.AppendUint64(nil, uint64(16+len(payload))), payload)
}

func (n mp4Node) bytes() []byte { return mp4Box(n.typ, n.payload) }

func mp4Render(nodes []mp4Node) []byte {
	var out []byte
	for _, n := range nodes {
		out = append(out, n.bytes()...)
	}
	return out
}

// mp4Children parses the boxes that tile b, with 32- or 64-bit sizes.
func mp4Children(t testing.TB, b []byte) []mp4Node {
	t.Helper()
	var out []mp4Node
	for len(b) > 0 {
		if len(b) < 8 {
			t.Fatalf("a truncated box header: %x", b)
		}
		size, hdr := uint64(binary.BigEndian.Uint32(b)), uint64(8)
		if size == 1 {
			size, hdr = binary.BigEndian.Uint64(b[8:]), 16
		}
		if size < hdr || size > uint64(len(b)) {
			t.Fatalf("box %q of size %d in %d bytes", b[4:8], size, len(b))
		}
		out = append(out, mp4Node{string(b[4:8]), bytes.Clone(b[hdr:size]), bytes.Clone(b[:size])})
		b = b[size:]
	}
	return out
}

// mp4Prefix is how many payload bytes of a container come before its
// children: the version and flags of a full "meta" box.
func mp4Prefix(n mp4Node) int {
	if n.typ != "meta" || len(n.payload) < 8 {
		return 0
	}
	switch string(n.payload[4:8]) {
	case "hdlr", "ilst", "mhdr", "ctry", "lang":
		return 0
	}
	return 4
}

// mp4Find returns the children of the container at path below nodes, nil
// when it is missing.
func mp4Find(t testing.TB, nodes []mp4Node, path ...string) []mp4Node {
	t.Helper()
	for _, typ := range path {
		i := slices.IndexFunc(nodes, func(n mp4Node) bool { return n.typ == typ })
		if i < 0 {
			return nil
		}
		nodes = mp4Children(t, nodes[i].payload[mp4Prefix(nodes[i]):])
	}
	return nodes
}

// iTunesHdlr is the handler of iTunes metadata, as iTunes writes it.
var iTunesHdlr = mp4Box("hdlr", make([]byte, 8), []byte("mdirappl"), make([]byte, 9))

// mp4Edit replaces the children of the container at path below nodes by
// fn's result, creating the containers of the path that are missing (a
// "meta" with its iTunes handler).
func mp4Edit(t testing.TB, nodes []mp4Node, path []string, fn func([]mp4Node) []mp4Node) []mp4Node {
	t.Helper()
	if len(path) == 0 {
		return fn(nodes)
	}
	out := slices.Clone(nodes)
	i := slices.IndexFunc(out, func(n mp4Node) bool { return n.typ == path[0] })
	if i < 0 {
		n := mp4Node{typ: path[0]}
		if path[0] == "meta" {
			n.payload = slices.Concat(make([]byte, 4), iTunesHdlr)
		}
		out = append(out, n)
		i = len(out) - 1
	}
	k := mp4Prefix(out[i])
	kids := mp4Edit(t, mp4Children(t, out[i].payload[k:]), path[1:], fn)
	out[i].payload = slices.Concat(out[i].payload[:k], mp4Render(kids))
	return out
}

var stblPath = []string{"trak", "mdia", "minf", "stbl"}

// mp4RewriteMoov rebuilds the file with the children of its moov replaced by
// fn's result, and fixes the chunk offsets that point past the old moov when
// its size changes, as a tagger does.
func mp4RewriteMoov(t testing.TB, file []byte, fn func([]mp4Node) []mp4Node) []byte {
	t.Helper()
	top := mp4Children(t, file)
	mi := slices.IndexFunc(top, func(n mp4Node) bool { return n.typ == "moov" })
	oldEnd := 0
	for _, n := range top[:mi+1] {
		oldEnd += len(n.bytes())
	}
	kids := fn(mp4Children(t, top[mi].payload))
	delta := len(mp4Box("moov", mp4Render(kids))) - len(top[mi].bytes())
	if delta != 0 {
		kids = mp4Edit(t, kids, stblPath, func(stbl []mp4Node) []mp4Node {
			for i, n := range stbl {
				if n.typ == "stco" || n.typ == "co64" {
					stbl[i].payload = shiftOffsets(n, uint64(oldEnd), int64(delta))
				}
			}
			return stbl
		})
	}
	top[mi].payload = mp4Render(kids)
	return mp4Render(top)
}

func shiftOffsets(n mp4Node, from uint64, delta int64) []byte {
	p := bytes.Clone(n.payload)
	count := int(binary.BigEndian.Uint32(p[4:]))
	for i := range count {
		if n.typ == "co64" {
			o := binary.BigEndian.Uint64(p[8+8*i:])
			if o >= from {
				binary.BigEndian.PutUint64(p[8+8*i:], uint64(int64(o)+delta))
			}
			continue
		}
		o := binary.BigEndian.Uint32(p[8+4*i:])
		if uint64(o) >= from {
			binary.BigEndian.PutUint32(p[8+4*i:], uint32(int64(o)+delta))
		}
	}
	return p
}

// withIlst returns the file with the given items as its ilst, followed by a
// free box of pad bytes when pad > 0; the other children of meta are kept
// except its free boxes; udta and meta are created when missing.
func withIlst(t testing.TB, file []byte, pad int, items ...[]byte) []byte {
	t.Helper()
	return mp4RewriteMoov(t, file, func(moov []mp4Node) []mp4Node {
		return mp4Edit(t, moov, []string{"udta", "meta"}, func(meta []mp4Node) []mp4Node {
			var out []mp4Node
			for _, n := range meta {
				if n.typ != "ilst" && n.typ != "free" {
					out = append(out, n)
				}
			}
			out = append(out, mp4Node{typ: "ilst", payload: slices.Concat(items...)})
			if pad > 0 {
				out = append(out, mp4Node{typ: "free", payload: make([]byte, pad-8)})
			}
			return out
		})
	})
}

// withoutUdta returns the file without its moov/udta.
func withoutUdta(t testing.TB, file []byte) []byte {
	t.Helper()
	return mp4RewriteMoov(t, file, func(moov []mp4Node) []mp4Node {
		return slices.DeleteFunc(moov, func(n mp4Node) bool { return n.typ == "udta" })
	})
}

// toCo64 returns the file with its stco box replaced by the same offsets in
// a co64 box.
func toCo64(t testing.TB, file []byte) []byte {
	t.Helper()
	return mp4RewriteMoov(t, file, func(moov []mp4Node) []mp4Node {
		return mp4Edit(t, moov, stblPath, func(stbl []mp4Node) []mp4Node {
			for i, n := range stbl {
				if n.typ != "stco" {
					continue
				}
				count := int(binary.BigEndian.Uint32(n.payload[4:]))
				p := slices.Clone(n.payload[:8])
				for k := range count {
					p = binary.BigEndian.AppendUint64(p, uint64(binary.BigEndian.Uint32(n.payload[8+4*k:])))
				}
				stbl[i] = mp4Node{typ: "co64", payload: p}
			}
			return stbl
		})
	})
}

// ilstItems returns the items of the file's moov/udta/meta/ilst, rendered.
func ilstItems(t testing.TB, file []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for _, n := range mp4Find(t, mp4Children(t, file), "moov", "udta", "meta", "ilst") {
		out = append(out, n.raw)
	}
	return out
}

// mp4Chunks returns the chunk offsets and sizes of the file's one track,
// read from its stsc, stsz and stco or co64 boxes.
func mp4Chunks(t testing.TB, file []byte) (offsets, sizes []uint64) {
	t.Helper()
	stbl := mp4Find(t, mp4Children(t, file), append([]string{"moov"}, stblPath...)...)
	get := func(typ string) []byte {
		for _, n := range stbl {
			if n.typ == typ {
				return n.payload
			}
		}
		return nil
	}
	if co := get("co64"); co != nil {
		for i := range int(binary.BigEndian.Uint32(co[4:])) {
			offsets = append(offsets, binary.BigEndian.Uint64(co[8+8*i:]))
		}
	} else {
		co := get("stco")
		for i := range int(binary.BigEndian.Uint32(co[4:])) {
			offsets = append(offsets, uint64(binary.BigEndian.Uint32(co[8+4*i:])))
		}
	}
	stsc, stsz := get("stsc"), get("stsz")
	uniform := binary.BigEndian.Uint32(stsz[4:])
	sample := 0
	runs := int(binary.BigEndian.Uint32(stsc[4:]))
	for c := range offsets {
		perChunk := 0
		for r := range runs {
			if int(binary.BigEndian.Uint32(stsc[8+12*r:])) <= c+1 {
				perChunk = int(binary.BigEndian.Uint32(stsc[8+12*r+4:]))
			}
		}
		size := uint64(0)
		for range perChunk {
			if uniform != 0 {
				size += uint64(uniform)
			} else {
				size += uint64(binary.BigEndian.Uint32(stsz[12+4*sample:]))
			}
			sample++
		}
		sizes = append(sizes, size)
	}
	return offsets, sizes
}

// mp4Samples returns the bytes of every sample of the file's track, in
// order: what a decoder reads, wherever the chunks are.
func mp4Samples(t testing.TB, file []byte) []byte {
	t.Helper()
	offsets, sizes := mp4Chunks(t, file)
	var out []byte
	for i, o := range offsets {
		out = append(out, file[o:o+sizes[i]]...)
	}
	return out
}

// mp4TopTypes lists the types of the top-level boxes.
func mp4TopTypes(t testing.TB, file []byte) []string {
	t.Helper()
	var out []string
	for _, n := range mp4Children(t, file) {
		out = append(out, n.typ)
	}
	return out
}

// Atom names of the iTunes list ("\xa9" is the "©" byte).
const (
	atomNam = "\xa9nam"
	atomART = "\xa9ART"
	atomAlb = "\xa9alb"
	atomDay = "\xa9day"
	atomGen = "\xa9gen"
	atomCmt = "\xa9cmt"
	atomWrt = "\xa9wrt"
	atomLyr = "\xa9lyr"
	atomToo = "\xa9too"
)

// dataAtom is a "data" atom of the type, with locale 0.
func dataAtom(typ uint32, v []byte) []byte { return mp4Box("data", be32(typ), be32(0), v) }

// textItem is an item whose values are UTF-8 data atoms.
func textItem(name string, values ...string) []byte {
	var kids [][]byte
	for _, v := range values {
		kids = append(kids, dataAtom(1, []byte(v)))
	}
	return mp4Box(name, kids...)
}

// pairItem is trkn (8 bytes) or disk (6 bytes).
func pairItem(name string, n, total uint16) []byte {
	v := slices.Concat([]byte{0, 0}, binary.BigEndian.AppendUint16(nil, n), binary.BigEndian.AppendUint16(nil, total))
	if name == "trkn" {
		v = append(v, 0, 0)
	}
	return mp4Box(name, dataAtom(0, v))
}

// intItem is an integer item of type 21 with the given bytes.
func intItem(name string, v ...byte) []byte { return mp4Box(name, dataAtom(21, v)) }

// freeformItem is a "----" item: mean, name, then its data atoms.
func freeformItem(mean, name string, data ...[]byte) []byte {
	return mp4Box("----", slices.Concat(mp4Box("mean", be32(0), []byte(mean)), mp4Box("name", be32(0), []byte(name)), slices.Concat(data...)))
}

// iTunesText is an iTunes freeform item of UTF-8 values.
func iTunesText(name string, values ...string) []byte {
	var data [][]byte
	for _, v := range values {
		data = append(data, dataAtom(1, []byte(v)))
	}
	return freeformItem("com.apple.iTunes", name, data...)
}
