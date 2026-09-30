package volume

import (
	"musiclib/internal/fsops"
)

// probeRenameExchange is fsops.ProbeRenameExchange; tests replace it to
// inject the failures that need a filesystem without RENAME_EXCHANGE.
var probeRenameExchange = fsops.ProbeRenameExchange

// CheckFilesystem runs the boot checks of the volume (DESIGN.md §3.1, §11.1
// step 3) on the layout opened by [Volume.OpenLayout]. Each failure is fatal
// and has its own code; there is no fallback for an unsuitable filesystem.
//
//  1. originals/, library/ and work/ have the st_dev of /data
//     ([CodeCrossDevice]) and are on the same mount as /data
//     ([CodeNestedMount]): all of /data is one filesystem with no nested
//     mounts, so publication (work -> library) and blob pinning
//     (work/blobs -> originals) are plain renames. Equal st_dev alone does
//     not exclude a bind mount of the same filesystem (N-032).
//  2. The process can list, traverse and create entries in /data and in
//     the three media directories ([CodePermission]).
//  3. renameat2(RENAME_EXCHANGE) really swaps two directories inside work/
//     ([CodeNoRenameExchange]). The probe runs only in work/, never in
//     library/, which is output only (N-033); check 1 already proves that
//     library/ and work/ are on the same mount.
func (v *Volume) CheckFilesystem() error {
	if v.originals == nil {
		return newErr(CodeIO, "CheckFilesystem called before OpenLayout", nil)
	}
	return checkFilesystem(v.root, v.originals, v.library, v.work)
}

func checkFilesystem(data, originals, library, work *fsops.Root) error {
	for _, r := range []*fsops.Root{originals, library, work} {
		same, err := fsops.SameFilesystem(data, r)
		if err != nil {
			return fsErr("cannot stat "+r.Name(), err)
		}
		if !same {
			return newErr(CodeCrossDevice, r.Name()+" is not on the filesystem of "+data.Name()+
				": all of /data must be one ext4 filesystem (DESIGN.md §3.1)", nil)
		}
		same, err = fsops.SameMount(data, r)
		if err != nil {
			return fsErr("cannot read the mount of "+r.Name(), err)
		}
		if !same {
			return newErr(CodeNestedMount, r.Name()+" is a separate mount inside "+data.Name()+
				": no nested mounts in originals, library or work (DESIGN.md §3.1)", nil)
		}
	}
	for _, r := range []*fsops.Root{data, originals, library, work} {
		if err := r.CheckAccess(true); err != nil {
			return fsErr("cannot read and write "+r.Name(), err)
		}
	}
	if err := probeRenameExchange(work, work); err != nil {
		switch fsops.Code(err) {
		case fsops.CodeUnsupportedOp, fsops.CodeCrossDevice:
			return newErr(CodeNoRenameExchange, "renameat2(RENAME_EXCHANGE) does not work in "+work.Name()+
				": Linux and ext4 are requirements, with no fallback (DESIGN.md §3.1)", err)
		default:
			return fsErr("the RENAME_EXCHANGE probe in "+work.Name()+" failed", err)
		}
	}
	return nil
}
