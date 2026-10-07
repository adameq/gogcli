package cmd

import (
	"testing"

	"github.com/openclaw/gogcli/internal/googleauth"
)

func TestPhotosNarrowsGrant(t *testing.T) {
	photos := []googleauth.Service{googleauth.ServiceGmail, googleauth.ServicePhotos}
	noPhotos := []googleauth.Service{googleauth.ServiceGmail}

	if !photosNarrowsGrant(photos, "readonly") {
		t.Fatal("photos readonly must disable include_granted_scopes")
	}

	if !photosNarrowsGrant(photos, "") {
		t.Fatal("photos default (read-only) must disable include_granted_scopes")
	}

	if photosNarrowsGrant(photos, "append") {
		t.Fatal("photos append may keep include_granted_scopes")
	}

	if photosNarrowsGrant(noPhotos, "readonly") {
		t.Fatal("no photos service: nothing to narrow")
	}
}
