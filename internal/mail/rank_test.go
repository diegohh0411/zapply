package mail

import "testing"

func TestSenderParts(t *testing.T) {
	for _, c := range []struct{ from, addr, org string }{
		{"Enrico Deusebio <enrico.deusebio@canonical.com>", "enrico.deusebio@canonical.com", "canonical"},
		{"jobs@mail.stripe.com", "jobs@mail.stripe.com", "stripe"},
		{"hr@empresa.com.mx", "hr@empresa.com.mx", "empresa"},
		{"no-reply@greenhouse-mail.io", "no-reply@greenhouse-mail.io", ""},
		{"someone@gmail.com", "someone@gmail.com", ""},
		{"not an address", "", ""},
	} {
		a, o := senderParts(c.from)
		if a != c.addr || o != c.org {
			t.Errorf("senderParts(%q) = %q, %q; want %q, %q", c.from, a, o, c.addr, c.org)
		}
	}
}

func TestCompanyScore(t *testing.T) {
	text := normalize("enrico.deusebio@canonical.com Canonical Interview Confirmation Dear Diego, the Starcraft Software Engineer role at Canonical")
	if got := companyScore("Canonical Ltd.", text, "canonical"); got != 30 {
		t.Errorf("named and sender domain: got %d, want 30", got)
	}
	if got := companyScore("Stripe", text, "canonical"); got != 0 {
		t.Errorf("unrelated company: got %d, want 0", got)
	}
	if got := companyScore("Can", text, "canonical"); got != 0 {
		t.Errorf("short partial name must not match: got %d", got)
	}
	if got := companyScore("Thomson Reuters Mexico", normalize("Your application"), "thomsonreuters"); got != 20 {
		t.Errorf("domain prefix of a multi-word company: got %d, want 20", got)
	}
}
