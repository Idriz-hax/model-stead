package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDeviceLifecycleRetainsUntrustedDevice(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES('u1','user1','User','user','active')`); err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDevice(ctx, "admin", "u1", "workstation", "192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UntrustDevice(ctx, "admin", d.ID); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListDevices(ctx)
	if err != nil || len(items) != 1 || items[0].Trusted {
		t.Fatalf("devices=%+v err=%v", items, err)
	}
	if err = s.TrustDevice(ctx, "admin", d.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateDevice(ctx, "admin", d.ID, "u1", "laptop", "2001:db8::1"); err != nil {
		t.Fatal(err)
	}
	items, err = s.ListDevices(ctx)
	if err != nil || items[0].IPAddress != "2001:db8::1" || !items[0].Trusted {
		t.Fatalf("devices=%+v err=%v", items, err)
	}
	if _, err = s.CreateDevice(ctx, "admin", "u1", "bad", "192.0.2.0/24"); err == nil {
		t.Fatal("accepted CIDR")
	}
}

func TestDeviceCanonicalIPUniquenessIsAudited(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, err := s.CreateDevice(ctx, "admin", "", "first", "192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	if first.IPAddress != "192.0.2.7" {
		t.Fatalf("IP not canonicalized: %q", first.IPAddress)
	}
	if _, err = s.CreateDevice(ctx, "admin", "", "duplicate", "::ffff:192.0.2.7"); err == nil {
		t.Fatal("accepted equivalent canonical IP")
	}
	devices, err := s.ListDevices(ctx)
	if err != nil || len(devices) != 1 || devices[0].IPAddress != first.IPAddress {
		t.Fatalf("devices=%+v err=%v", devices, err)
	}
	events, err := s.ListAudit(ctx, 0, 100)
	if err != nil || len(events) != 1 || events[0].Action != "device.create" || events[0].After.IPAddress != first.IPAddress {
		t.Fatalf("audit=%+v err=%v", events, err)
	}
}

func TestResourcePaginationAndDeviceLookup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i, ip := range []string{"192.0.2.10", "192.0.2.11", "192.0.2.12"} {
		if _, err := s.CreateDevice(ctx, "admin", "", string(rune('a'+i)), ip); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ListDevicesPage(ctx, 1, 1)
	if err != nil || len(page) != 1 || page[0].IPAddress != "192.0.2.11" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	got, err := s.GetDevice(ctx, page[0].ID)
	if err != nil || got.ID != page[0].ID {
		t.Fatalf("device=%+v err=%v", got, err)
	}
	deviceJSON, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(deviceJSON), `"last_seen_at":null`) {
		t.Fatalf("device DTO=%s err=%v", deviceJSON, err)
	}
	if _, err = s.ListDevicesPage(ctx, 101, 0); err == nil {
		t.Fatal("accepted oversized page")
	}
	if _, err = s.ListDevicesPage(ctx, 10, -1); err == nil {
		t.Fatal("accepted negative offset")
	}
	if _, err = s.ListDevicesPage(ctx, 0, 0); err != nil {
		t.Fatalf("default page size: %v", err)
	}
}

func TestAPIKeyLifecycleAndRedaction(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES('u1','user1','User','user','active')`); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	k, secret, err := s.CreateAPIKey(ctx, "admin", "u1", "automation", &expires)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "afr_v1_") || strings.Contains(k.KeyPrefix, secret) {
		t.Fatal("invalid key response")
	}
	list, err := s.ListAPIKeys(ctx, "u1")
	if err != nil || len(list) != 1 || list[0].ID != k.ID {
		t.Fatalf("keys=%+v err=%v", list, err)
	}
	all, err := s.ListAPIKeysPage(ctx, "", 50, 0)
	if err != nil || len(all) != 1 || all[0].UserID != "u1" {
		t.Fatalf("all keys=%+v err=%v", all, err)
	}
	if _, err = s.ListAPIKeysPage(ctx, "", 101, 0); err == nil {
		t.Fatal("accepted oversized key page")
	}
	if _, err = s.ListAPIKeysPage(ctx, "", 20, -1); err == nil {
		t.Fatal("accepted negative key offset")
	}
	var hash string
	if err = s.DB.QueryRow(`SELECT key_hash FROM api_keys WHERE id=?`, k.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == secret || strings.Contains(hash, secret) {
		t.Fatal("secret persisted")
	}
	listedJSON, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(listedJSON), hash) || strings.Contains(string(listedJSON), secret) {
		t.Fatalf("listed API key exposes secret material: %s", listedJSON)
	}
	if strings.Contains(string(listedJSON), `"expires_at":{"`) || !strings.Contains(string(listedJSON), `"revoked_at":null`) || !strings.Contains(string(listedJSON), `"last_used_at":null`) {
		t.Fatalf("nullable API key fields have invalid JSON shape: %s", listedJSON)
	}
	if err = s.RevokeAPIKey(ctx, "admin", "u1", k.ID); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListAudit(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		encoded, marshalErr := json.Marshal(e)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(encoded), hash) || strings.Contains(string(encoded), secret) {
			t.Fatalf("audit event exposes key hash or secret: %s", encoded)
		}
	}
	if _, _, err = s.CreateAPIKey(ctx, "admin", "u1", "", nil); err == nil {
		t.Fatal("accepted empty name")
	}
}
