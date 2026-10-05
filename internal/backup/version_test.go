package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/project"
)

// TestBackupVersionsThisBuildWritesAndReads pins the format's version rule from both sides. A file
// this build writes names version 3, so a release that reads only version 2 refuses it rather than
// dropping the notification targets, callback keys, and sealed survey defaults it does not know and
// reporting a complete restore. A version 2 file, which a previous release wrote and which holds
// none of those, still restores here, so an install moving forward keeps its backups.
func TestBackupVersionsThisBuildWritesAndReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	if err := src.Projects.Save(ctx, &project.Project{ID: "prj_kept", Name: "kept",
		RepoURL: "https://example.com/kept.git"}); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	var written bytes.Buffer
	if _, err := Write(ctx, src, testSealerOnce(), &written); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	var env envelope
	if err := json.Unmarshal(written.Bytes(), &env); err != nil {
		t.Fatalf("decode the written envelope: %v", err)
	}
	if env.Version != 3 {
		t.Errorf("Write() stamped version %d, want 3, the version a reader of 2 refuses", env.Version)
	}

	stamp := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	inner, err := json.Marshal(payload{CreatedAt: stamp, Projects: []*project.Project{{
		ID: "prj_old", Name: "old", RepoURL: "https://example.com/old.git"}}})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	tests := []struct {
		Version   int
		WantCount int
		Want      error
	}{{ // Test 0: A file a previous release wrote restores whole.
		Version: 2, WantCount: 1,
	}, { // Test 1: A file this release writes restores.
		Version: 3, WantCount: 1,
	}, { // Test 2: Version 1 carried an unauthenticated header and is still refused.
		Version: 1, Want: ErrFormat,
	}, { // Test 3: A version newer than this build is refused, as this build is by the one before.
		Version: 4, Want: ErrFormat,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sealed, err := testSealerOnce().Seal(string(testGzip(t, inner)))
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			file, err := json.Marshal(envelope{Format: Format, Version: test.Version,
				CreatedAt: stamp, Sealed: sealed})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			dst := freshStores()
			sum, err := Read(ctx, dst, testSealerOnce(), bytes.NewReader(file))
			if !errors.Is(err, test.Want) {
				t.Fatalf("Read() of a version %d file error = %v, want %v", test.Version, err,
					test.Want)
			}
			if sum.Projects != test.WantCount {
				t.Errorf("Read() of a version %d file restored %d projects, want %d", test.Version,
					sum.Projects, test.WantCount)
			}
		})
	}
}
