# Important implementation notes

## LOC Video Frame Marking for AV1 SVC

`locPropFrameMark = 0x04` identifies the LOC Video Frame Marking property.
Property ID `0x04` comes from the LOC specification; it is not a project-local
identifier.

Each published AV1 SVC object carries an RFC 9626 frame marking so relays and
subscribers can identify frame boundaries, sync frames, and spatial layers
without parsing the AV1 payload. Catalog `spatialId` describes the track;
frame marking supplies equivalent information for each object and enables
layer-aware forwarding.

The two marking octets are encoded in the least-significant bits of the LOC
`vi64` value:

```text
[S E I D B TID][LID]
```

Current spatial-only mapping:

```text
S=1 E=1 I=source_sync D=0 B=0 TID=0 LID=spatial_id
```

`TL0PICIDX` is omitted. Example values:

```text
s0 delta: 0xc000
s0 sync:  0xe000
s1 delta: 0xc001
s2 sync:  0xe002
```

Removing this property would not stop AV1 payload delivery, and catalog layer
dependencies would remain. It would remove per-object layer and independence
signals, prevent codec-agnostic layer-aware forwarding, and violate the chosen
LOC SVC publishing design.
