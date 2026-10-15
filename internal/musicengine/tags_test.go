package musicengine

import "testing"

func TestParseTagsReadsFLACAndMP4Names(t *testing.T) {
	flac := []byte(`{"format":{"duration":"201.5","tags":{
		"MUSICBRAINZ_TRACKID":"11111111-2222-3333-4444-555555555555",
		"MUSICBRAINZ_RELEASETRACKID":"99999999-2222-3333-4444-555555555555",
		"MUSICBRAINZ_ALBUMID":"AAAAAAAA-2222-3333-4444-555555555555",
		"ISRC":"us-abc-10-00001","TITLE":"Song","ARTIST":"Artist","ALBUM":"Album"}}}`)
	got, err := parseTags(flac)
	if err != nil {
		t.Fatal(err)
	}
	if got.RecordingMBID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("recording id = %q (the release-track id must not be read)", got.RecordingMBID)
	}
	if got.ReleaseMBID != "aaaaaaaa-2222-3333-4444-555555555555" || got.ISRC != "USABC1000001" ||
		got.Title != "Song" || got.Artist != "Artist" || got.Album != "Album" || got.DurationSec != 201.5 {
		t.Errorf("tags = %+v", got)
	}

	mp4 := []byte(`{"format":{"tags":{"MusicBrainz Track Id":"11111111-2222-3333-4444-555555555555","album_artist":"AA"}}}`)
	got, err = parseTags(mp4)
	if err != nil || got.RecordingMBID == "" || got.Artist != "AA" {
		t.Errorf("mp4 tags = %+v err %v", got, err)
	}
}

func TestParseTagsIgnoresMalformedIdsAndRejectsBadJSON(t *testing.T) {
	got, err := parseTags([]byte(`{"format":{"tags":{"MUSICBRAINZ_TRACKID":"not-a-uuid"}}}`))
	if err != nil || got.RecordingMBID != "" {
		t.Errorf("tags = %+v err %v", got, err)
	}
	if _, err := parseTags([]byte(`not json`)); err == nil {
		t.Error("expected an error for unreadable ffprobe output")
	}
}
