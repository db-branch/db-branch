package zfs_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/zfs"
)

func TestTree_データセットとスナップショットの使用量を一度に読む(t *testing.T) {
	f := &fakeRunner{out: []byte("" +
		"tank/db-branch\tfilesystem\t3000\t100\t0\t-\t1700000000\t-\n" +
		"tank/db-branch/bases/orders\tfilesystem\t2000\t1500\t120\t-\t1700000001\t-\n" +
		"tank/db-branch/bases/orders@s1\tsnapshot\t100\t1500\t-\t-\t1700000100\ttank/db-branch/branches/x,tank/db-branch/branches/y\n" +
		"tank/db-branch/bases/orders@s2\tsnapshot\t20\t1600\t-\t-\t1700000200\t-\n" +
		"tank/db-branch/branches/x\tfilesystem\t50\t1500\t0\ttank/db-branch/bases/orders@s1\t1700000300\t-\n")}
	got, err := zfs.New(f).Tree(context.Background(), "tank/db-branch")
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "list", "-Hp", "-r", "-t", "filesystem,snapshot", "-o", "name,type,used,referenced,usedbysnapshots,origin,creation,clones", "tank/db-branch"})
	want := []domain.SpaceNode{
		{Name: "tank/db-branch", Kind: "filesystem", Used: 3000, Referenced: 100, Created: time.Unix(1700000000, 0).UTC()},
		{Name: "tank/db-branch/bases/orders", Kind: "filesystem", Used: 2000, Referenced: 1500, UsedBySnapshots: 120, Created: time.Unix(1700000001, 0).UTC()},
		{Name: "tank/db-branch/bases/orders@s1", Kind: "snapshot", Used: 100, Referenced: 1500, Created: time.Unix(1700000100, 0).UTC(), Clones: []string{"tank/db-branch/branches/x", "tank/db-branch/branches/y"}},
		{Name: "tank/db-branch/bases/orders@s2", Kind: "snapshot", Used: 20, Referenced: 1600, Created: time.Unix(1700000200, 0).UTC()},
		{Name: "tank/db-branch/branches/x", Kind: "filesystem", Used: 50, Referenced: 1500, Origin: "tank/db-branch/bases/orders@s1", Created: time.Unix(1700000300, 0).UTC()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestTree_不正な名前や読めない行は失敗する(t *testing.T) {
	if _, err := zfs.New(&fakeRunner{}).Tree(context.Background(), "-bad"); err == nil {
		t.Error("an option-like root is refused")
	}
	for _, line := range []string{"only\ttwo\n", "n\tfilesystem\tx\t1\t0\t-\t1\t-\n", "n\tfilesystem\t1\t1\t0\t-\tsoon\t-\n"} {
		if _, err := zfs.New(&fakeRunner{out: []byte(line)}).Tree(context.Background(), "tank/db-branch"); err == nil {
			t.Errorf("%q should fail", line)
		}
	}
	if _, err := zfs.New(&fakeRunner{err: errors.New("boom")}).Tree(context.Background(), "tank/db-branch"); err == nil {
		t.Error("a failing zfs fails")
	}
}

func TestSpace_プールの大きさと空きを返す(t *testing.T) {
	f := &fakeRunner{out: []byte("3000\t7000\n")}
	size, free, err := zfs.New(f).Space(context.Background(), "tank")
	if err != nil || size != 10000 || free != 7000 {
		t.Fatalf("%d %d %v", size, free, err)
	}
	assertCalls(t, f, call{"zfs", "list", "-Hp", "-o", "used,available", "tank"})
	if _, _, err := zfs.New(&fakeRunner{out: []byte("x\ty\n")}).Space(context.Background(), "tank"); err == nil {
		t.Error("unreadable output fails")
	}
}
