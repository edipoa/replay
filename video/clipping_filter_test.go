package video

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func filterOf(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "-filter_complex" {
			return args[i+1]
		}
	}
	t.Fatalf("no -filter_complex in %v", args)
	return ""
}

func TestBuildOverlayArgs(t *testing.T) {
	const wmStage = "[1:v]scale=-1:44[ov1];[cur0][ov1]overlay=10:H-h-10[cur1]"
	const lgStage = "[2:v]scale=80:-1[ov2];[cur1][ov2]overlay=W-w-10:H-h-10[cur2]"

	if got := buildOverlayArgs(1280, "wm.png", "lg.png", nil, false, false); !reflect.DeepEqual(got, []string{"-vf", "scale=1280:-2"}) {
		t.Fatalf("none: %v", got)
	}

	got := buildOverlayArgs(1280, "wm.png", "lg.png", nil, true, false)
	want := []string{"-i", "wm.png", "-filter_complex",
		"[0:v]scale=1280:-2[cur0];" + wmStage + ";[cur1]copy[outv]", "-map", "[outv]", "-map", "0:a?"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("watermark only:\n%#v\nwant\n%#v", got, want)
	}

	got = buildOverlayArgs(1280, "wm.png", "lg.png", nil, true, true)
	want = []string{"-i", "wm.png", "-i", "lg.png", "-filter_complex",
		"[0:v]scale=1280:-2[cur0];" + wmStage + ";" + lgStage + ";[cur2]copy[outv]", "-map", "[outv]", "-map", "0:a?"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("watermark+logo:\n%#v\nwant\n%#v", got, want)
	}
	if strings.Contains(filterOf(t, got), "pad=iw") {
		t.Fatal("footer pad present without sponsors")
	}

	// 4 sponsors: totalW = 4*140+3*24 = 632, startX = (1280-632)/2 = 324, step 164.
	sp := []string{"a.png", "b.png", "c.png", "d.png"}
	fc := filterOf(t, buildOverlayArgs(1280, "wm.png", "lg.png", sp, true, true))
	if !strings.Contains(fc, "[cur2]pad=iw:ih+80:0:0:0x0E2A5E[cur3]") {
		t.Errorf("missing footer pad: %s", fc)
	}
	const fit = "scale=140:60:force_original_aspect_ratio=decrease,format=rgba,pad=140:60:(ow-iw)/2:(oh-ih)/2:color=black@0.0"
	for i, x := range []int{324, 488, 652, 816} {
		n := i + 3 // input index and current label of this sponsor's stage
		stage := fmt.Sprintf("[%d:v]%s[ov%d];[cur%d][ov%d]overlay=%d:H-h-10[cur%d]", n, fit, n, n, n, x, n+1)
		if !strings.Contains(fc, stage) {
			t.Errorf("sponsor %d: missing %q in %s", i, stage, fc)
		}
	}
	if !strings.HasSuffix(fc, "[cur7]copy[outv]") {
		t.Errorf("bad tail: %s", fc)
	}

	// Narrow clip: startX clamps to 10.
	if fc := filterOf(t, buildOverlayArgs(400, "", "", sp, false, false)); !strings.Contains(fc, "overlay=10:H-h-10") {
		t.Errorf("startX not clamped to 10: %s", fc)
	}
}
