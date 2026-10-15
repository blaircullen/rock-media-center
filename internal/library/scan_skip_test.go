package library

import "testing"

// Hidden folders (the recycle bin included) are never scanned.
func TestSkipScanDirSkipsRecycleDir(t *testing.T) {
	if !SkipScanDir(RecycleDirName) || !SkipScanDir(".AppleDouble") || SkipScanDir("Season 01") {
		t.Error("SkipScanDir must skip hidden folders and only those")
	}
}
