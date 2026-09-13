package email

import (
	"strings"
	"testing"
)

// The bodies are the only part of this package a stranger ever reads, and they
// are assembled by string concatenation, so the things worth pinning are the
// ones a careless edit would silently break: that the code survives, that
// attacker-controlled text cannot become markup, and that no internal
// identifier leaks into copy.

func TestOTPBodyCarriesTheCode(t *testing.T) {
	body := otpBody("804710")
	if !strings.Contains(body, "804710") {
		t.Fatal("otp body does not contain the code")
	}
	if !strings.Contains(body, "Expires in 10 minutes") {
		t.Error("otp body dropped the expiry line")
	}
}

func TestBodiesEscapeTheirInputs(t *testing.T) {
	// The code is generated, but the address is not: an invitation can be
	// created for whatever a super admin typed. If that reaches the recipient
	// as live markup, one account can post script into another's inbox.
	evil := `"><script>alert(1)</script>@x.test`

	body := invitationBody(evil, "organizer")
	if strings.Contains(body, "<script>") {
		t.Fatal("invitation body emitted an unescaped <script> from the address")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("invitation body did not escape the address at all")
	}

	if strings.Contains(otpBody(`<b>1</b>`), "<b>") {
		t.Error("otp body emitted unescaped markup from the code")
	}
}

func TestInvitationNamesTheRoleInEnglish(t *testing.T) {
	// This read "as an super_admin" before: wrong article, and a database
	// identifier shown to someone with no reason to know one exists.
	body := invitationBody("someone@example.test", "super_admin")
	if strings.Contains(body, "super_admin") {
		t.Error("invitation leaked the raw role identifier")
	}
	if !strings.Contains(body, "a super admin") {
		t.Error("invitation does not name the role readably")
	}
	if !strings.Contains(invitationBody("a@b.test", "organizer"), "an organizer") {
		t.Error("organizer should take 'an'")
	}
}

func TestArticleHandlesUnknownRoles(t *testing.T) {
	if got := article("tournament_referee"); got != "a tournament referee" {
		t.Errorf("unknown role should still read as English, got %q", got)
	}
}

func TestShellSurvivesEmailClients(t *testing.T) {
	out := shell("<div>hi</div>")

	// Outlook renders through Word: no flexbox, no grid. Tables or nothing.
	if !strings.Contains(out, "<table") {
		t.Error("frame must be table-based to survive Outlook")
	}
	// A <style> block would be stripped by several clients, and Plunk wraps
	// this fragment in its own document anyway.
	if strings.Contains(out, "<style") {
		t.Error("frame must not rely on a <style> block")
	}
	// Instrument Serif cannot load in an inbox; Georgia is the fallback
	// layout.css already declares, so the two stay in agreement.
	if !strings.Contains(out, "Georgia") {
		t.Error("frame must name a font that actually exists on the client")
	}
	if !strings.Contains(out, "#0d0d0f") || !strings.Contains(out, "#d4a94e") {
		t.Error("frame lost the theme's page background or accent")
	}
	if !strings.Contains(out, "<div>hi</div>") {
		t.Error("frame dropped the body it was given")
	}
}
