# AV1 spatial-SVC test asset

`video.mp4` is a 10-second, 25 fps, 1280x720 fragmented MP4 containing one
combined AV1 spatial-SVC stream. Each of its 250 temporal-unit samples contains
three spatial layers:

| Spatial ID | Encoded size | Cumulative target |
|-----------:|-------------:|------------------:|
| 0 | 320x180 | 150 kbps |
| 1 | 640x360 | 450 kbps |
| 2 | 1280x720 | 900 kbps |

The stream has one temporal layer and a one-second keyframe interval. Each
sample is its own MP4 fragment (timescale 12800, duration 512). Loading this
asset causes the LOC catalog to expose `video/s0`, `video/s1`, and `video/s2`;
CMSF keeps the combined `video` track.

Regenerate it from `utils/contentgen` using libaom's official reference encoder:

```bash
cd utils/contentgen
SVC_ENCODER_PATH=/path/to/svc_encoder_rtc go run videogen.go -codecs svc
cp output/video.mp4 ../../assets/testsvc/video.mp4
```

See `utils/contentgen/README.md` for encoder and packer details. The bundled
`mlmsub` does not yet merge the dependent LOC tracks.
