package storage

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openStageS3(t *testing.T, stub *stubS3, mutate func(*S3Config)) *S3 {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	cfg := S3Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		StageDir:        t.TempDir(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := OpenS3(cfg)
	if err != nil {
		t.Fatalf("OpenS3: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestS3StageCommitSmall(t *testing.T) {
	stub := newStubS3()
	s := openStageS3(t, stub, nil)
	ctx := context.Background()

	id, err := s.NewStage(ctx)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	if _, err := s.AppendToStage(ctx, id, 0, strings.NewReader("hello ")); err != nil {
		t.Fatalf("first append: %v", err)
	}
	size, err := s.AppendToStage(ctx, id, 6, strings.NewReader("world"))
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	if size != 11 {
		t.Errorf("stage size = %d, want 11", size)
	}

	if err := s.MkdirAll(ctx, MustParsePath("/docs")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := s.CommitStage(ctx, id, MustParsePath("/docs/readme.txt")); err != nil {
		t.Fatalf("CommitStage: %v", err)
	}
	if !stub.has("docs/readme.txt") {
		t.Fatal("the committed object is missing from the bucket")
	}
	info, err := s.Stat(ctx, MustParsePath("/docs/readme.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 11 {
		t.Errorf("committed size = %d, want 11", info.Size)
	}
	if _, err := s.StageSize(ctx, id); !errors.Is(err, ErrNoStagedFile) {
		t.Errorf("StageSize after commit = %v, want ErrNoStagedFile", err)
	}
}

func TestS3StageCommitMultipart(t *testing.T) {
	stub := newStubS3()
	s := openStageS3(t, stub, func(cfg *S3Config) { cfg.PartSize = 5 })
	ctx := context.Background()

	id, err := s.NewStage(ctx)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	payload := bytes.Repeat([]byte("zefile"), 3) // 18 bytes: parts 5, 5, 5, 3
	if _, err := s.AppendToStage(ctx, id, 0, bytes.NewReader(payload)); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := s.CommitStage(ctx, id, MustParsePath("/big.bin")); err != nil {
		t.Fatalf("CommitStage: %v", err)
	}
	info, err := s.Stat(ctx, MustParsePath("/big.bin"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("committed size = %d, want %d", info.Size, len(payload))
	}
}

func TestS3StageRefusals(t *testing.T) {
	stub := newStubS3("taken.txt")
	s := openStageS3(t, stub, nil)
	ctx := context.Background()

	if _, err := s.EnsureStage(ctx, "../escape"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("EnsureStage with a traversal id = %v, want ErrInvalidPath", err)
	}

	id, err := s.NewStage(ctx)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	if _, err := s.AppendToStage(ctx, id, 0, strings.NewReader("data")); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := s.CommitStage(ctx, id, MustParsePath("/taken.txt")); !errors.Is(err, ErrExist) {
		t.Errorf("CommitStage onto an existing file = %v, want ErrExist", err)
	}

	if _, err := s.AppendToStage(ctx, id, 1<<20, strings.NewReader("x")); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("append beyond the received bytes = %v, want ErrInvalidPath", err)
	}

	if err := s.DiscardStage(ctx, id); err != nil {
		t.Fatalf("DiscardStage: %v", err)
	}
	if err := s.DiscardStage(ctx, id); err != nil {
		t.Fatalf("DiscardStage again: %v", err)
	}
	if err := s.CommitStage(ctx, id, MustParsePath("/gone.txt")); !errors.Is(err, ErrNoStagedFile) {
		t.Errorf("CommitStage after discard = %v, want ErrNoStagedFile", err)
	}
}

func TestS3StagePurge(t *testing.T) {
	stub := newStubS3()
	s := openStageS3(t, stub, nil)
	ctx := context.Background()

	abandoned, err := s.NewStage(ctx)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(s.stageDir, string(abandoned)), old, old); err != nil {
		t.Fatalf("age the stage: %v", err)
	}
	fresh, err := s.NewStage(ctx)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}

	removed, err := s.PurgeStagesOlderThan(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("PurgeStagesOlderThan: %v", err)
	}
	if removed != 1 {
		t.Errorf("purged %d stages, want 1", removed)
	}
	if _, err := s.StageSize(ctx, fresh); err != nil {
		t.Errorf("the fresh stage was purged with the abandoned one: %v", err)
	}
}

func TestS3StageReadOnly(t *testing.T) {
	stub := newStubS3()
	s := openStageS3(t, stub, func(cfg *S3Config) { cfg.ReadOnly = true })
	if _, err := s.NewStage(context.Background()); !errors.Is(err, ErrReadOnly) {
		t.Errorf("NewStage read-only = %v, want ErrReadOnly", err)
	}
}
