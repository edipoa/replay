// Package selftest embeds a tiny real H264/MPEG-TS clip used to verify an
// ffmpeg binary can actually demux and decode media on the current host —
// "ffmpeg -version" succeeds even on builds that segfault on real files.
package selftest

import _ "embed"

//go:embed selftest.ts
var ClipTS []byte
