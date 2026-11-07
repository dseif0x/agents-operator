package store

import (
	"context"
	"testing"
)

func TestMemoryStores(t *testing.T) {
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u := &User{Username: "mem-owner-" + name, PasswordHash: "h"}
			if err := st.Users().Create(ctx, u); err != nil {
				t.Fatal(err)
			}
			s := &Session{OwnerID: u.ID, Name: "m", Agent: "claude", PVCSize: "1Gi", State: StateRunning}
			if err := st.Sessions().Create(ctx, s); err != nil {
				t.Fatal(err)
			}
			m := &AgentMemory{SessionID: s.ID, Text: "The owner likes short answers.", Source: "agent"}
			if err := st.Memories().Add(ctx, m); err != nil {
				t.Fatal(err)
			}
			if up, err := st.Memories().Update(ctx, s.ID, m.ID, "Short answers, please."); err != nil || up.Text != "Short answers, please." {
				t.Fatalf("update = %+v, %v", up, err)
			}
			if ms, _ := st.Memories().List(ctx, s.ID); len(ms) != 1 {
				t.Fatalf("list = %v", ms)
			}
			if err := st.Memories().Delete(ctx, s.ID, m.ID); err != nil {
				t.Fatal(err)
			}
			if ms, _ := st.Memories().List(ctx, s.ID); len(ms) != 0 {
				t.Fatalf("after delete = %v", ms)
			}
		})
	}
}
