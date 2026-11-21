package server

import (
	"testing"
	"time"
)

func TestDesktopCodesExpireAfterTwoMinutes(t *testing.T) {
	codes := newDesktopCodes()
	now := time.Now()
	fresh := codes.add(desktopCode{userID: "u", expires: now.Add(desktopCodeTTL)}, now)
	stale := codes.add(desktopCode{userID: "u", expires: now.Add(desktopCodeTTL)}, now)
	if _, ok := codes.take(fresh, now.Add(desktopCodeTTL-time.Second)); !ok {
		t.Fatal("code refused within two minutes")
	}
	if _, ok := codes.take(stale, now.Add(desktopCodeTTL)); ok {
		t.Fatal("code accepted after two minutes")
	}
	if len(codes.items) != 0 {
		t.Fatalf("taken codes still stored: %d", len(codes.items))
	}
}

func TestDesktopCodesStayBounded(t *testing.T) {
	codes := newDesktopCodes()
	now := time.Now()
	first := codes.add(desktopCode{expires: now.Add(time.Second)}, now)
	for i := 1; i < maxDesktopCodes; i++ {
		codes.add(desktopCode{expires: now.Add(desktopCodeTTL)}, now)
	}
	codes.add(desktopCode{expires: now.Add(desktopCodeTTL)}, now)
	if len(codes.items) != maxDesktopCodes {
		t.Fatalf("table holds %d codes", len(codes.items))
	}
	if _, ok := codes.items[first]; ok {
		t.Fatal("a full table kept its oldest code")
	}
}
