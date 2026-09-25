#include "inspection.h"

#include "failure.h"
#include "json.h"
#include "sha256.h"

namespace mltags {

namespace {

void writeKeyValues(json::Writer &w, const KeyValues &kv) {
  w.beginObject();
  w.key("key");
  w.string(kv.key);
  w.key("values");
  w.beginArray();
  for (const auto &v : kv.values) w.string(v);
  w.endArray();
  w.endObject();
}

}  // namespace

std::string renderInspection(const Inspection &in) {
  json::Writer w;
  w.beginObject();
  w.key("format");
  w.string(formatName(in.format));

  w.key("managed");
  w.beginObject();
  for (std::size_t i = 0; i < kFieldCount; ++i) {
    w.key(kFieldNames[i]);
    w.beginArray();
    for (const auto &v : in.managed[i]) w.string(v);
    w.endArray();
  }
  w.endObject();

  w.key("conflicts");
  w.beginArray();
  for (const auto &c : in.conflicts) {
    w.beginObject();
    w.key("field");
    w.string(kFieldNames[idx(c.field)]);
    w.key("sources");
    w.beginArray();
    for (const auto &s : c.sources) writeKeyValues(w, s);
    w.endArray();
    w.endObject();
  }
  w.endArray();

  w.key("pictures");
  w.beginArray();
  for (std::size_t i = 0; i < in.pictures.size(); ++i) {
    const Picture &p = in.pictures[i];
    w.beginObject();
    w.key("index");
    w.uinteger(i);
    w.key("location");
    w.string(p.location);
    w.key("type");
    w.uinteger(p.type);
    w.key("mime");
    w.string(p.mime);
    w.key("width");
    w.uinteger(p.width);
    w.key("height");
    w.uinteger(p.height);
    w.key("depth");
    w.uinteger(p.depth);
    w.key("colors");
    w.uinteger(p.colors);
    w.key("size");
    w.uinteger(p.data.size());
    w.key("sha256");
    w.string(sha256Hex(p.data));
    w.endObject();
  }
  w.endArray();

  w.key("unmanaged");
  w.beginArray();
  for (const auto &kv : in.unmanaged) writeKeyValues(w, kv);
  w.endArray();

  w.key("opaque");
  w.beginArray();
  for (const auto &o : in.opaque) {
    w.beginObject();
    w.key("key");
    w.string(o.key);
    w.key("reason");
    w.string(o.reason);
    w.key("removed");
    w.boolean(o.removed);
    w.endObject();
  }
  w.endArray();

  w.key("audio");
  w.beginObject();
  w.key("start");
  w.uinteger(in.audio.start);
  w.key("end");
  w.uinteger(in.audio.end);
  w.endObject();

  w.endObject();
  return w.take();
}

void refuseBlocking(const std::vector<Opaque> &opaque) {
  std::string keys;
  for (const auto &o : opaque) {
    if (o.removed) continue;
    if (!keys.empty()) keys += ", ";
    keys += o.key + " (" + o.reason + ")";
  }
  if (!keys.empty()) throw Failure(code::kOpaqueField, "fields that cannot be saved back without loss: " + keys);
}

}  // namespace mltags
