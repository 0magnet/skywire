package group

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.db")
	st, err := OpenStore(path, testStoreSK())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	dir := t.TempDir()
	m, err := NewManager(ManagerConfig{Store: st, DmsgC: dmsgStub, DataDir: dir})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	for _, r := range []Record{
		{ID: "left-1", Name: "gone", Role: RoleMember, Status: StatusLeft},
		{ID: "live-1", Name: "here", Role: RoleMember, Status: StatusActive},
	} {
		if err := st.Put(r); err != nil {
			t.Fatalf("Put %s: %v", r.ID, err)
		}
	}
	data := filepath.Join(dir, "group", "left-1", "member")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := m.Forget("live-1"); !errors.Is(err, ErrForgetLive) {
		t.Fatalf("Forget of an active group = %v, want ErrForgetLive", err)
	}
	if err := m.Forget("left-1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "group", "left-1")); !os.IsNotExist(err) {
		t.Errorf("CXO data of a forgotten group still on disk: %v", err)
	}
	if err := m.Forget("left-1"); err != nil {
		t.Errorf("a second Forget = %v, want nil", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = OpenStore(path, testStoreSK())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })  //nolint:errcheck
	if _, ok, _ := st.Get("left-1"); ok { //nolint:errcheck
		t.Error("a forgotten group came back after a restart")
	}
	if _, ok, _ := st.Get("live-1"); !ok { //nolint:errcheck
		t.Error("Forget dropped an active group")
	}
}
