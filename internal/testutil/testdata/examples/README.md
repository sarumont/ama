# Example fixtures

Reference captures showing the fixture layout described in
[`../../doc.go`](../../doc.go) and [`/testdata/README.md`](../../../../testdata/README.md).
They exist to exercise `FixtureRunner` and to give a new package something to
copy — they are **not** production test data, so no package should assert
against them.

**Every capture here is hand-constructed**, not recorded from a real disc: this
repository is developed without an optical drive or a MakeMKV license. Each
`cmd` file says so in a comment. The output is shaped to match the real tools
(makemkvcon robot mode `PREFIX:values`, ffprobe `-print_format json`, whipper's
plain-text TOC) but the values are invented, and the discs are not real releases.

Real fixtures recorded from hardware belong in the package that parses them —
`internal/bluray/testdata/...`, `internal/subtitle/testdata/...` — with a `cmd`
comment naming the tool version and capture date.

| Fixture | Demonstrates |
| --- | --- |
| `makemkvcon/info-disc0` | robot-mode disc scan: `DRV`/`TCOUNT`/`CINFO`/`TINFO`/`SINFO` records |
| `makemkvcon/info-no-disc` | a non-zero `exitcode` plus recorded `stderr` |
| `ffprobe/pgs-subtitles` | JSON stream list with two English PGS tracks of differing size |
| `whipper/cd-info` | plain-text CD TOC with a MusicBrainz disc ID |
