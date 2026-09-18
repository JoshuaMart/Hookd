package smtp

import "testing"

func TestHookIDFromRecipient(t *testing.T) {
	const domain = "hookd.example.com"

	tests := []struct {
		name    string
		addr    string
		wantID  string
		wantTag string
		wantOK  bool
	}{
		{"apex", "a3f9c2d1@hookd.example.com", "a3f9c2d1", "", true},
		{"angle brackets", "<a3f9c2d1@hookd.example.com>", "a3f9c2d1", "", true},
		{"sub-address tag", "a3f9c2d1+carrefour@hookd.example.com", "a3f9c2d1", "carrefour", true},
		{"empty tag", "a3f9c2d1+@hookd.example.com", "a3f9c2d1", "", true},
		{"tag with a plus", "a3f9c2d1+a+b@hookd.example.com", "a3f9c2d1", "a+b", true},
		{"uppercase", "<A3F9C2D1+TAG@HOOKD.EXAMPLE.COM>", "a3f9c2d1", "tag", true},
		{"surrounding whitespace", "  a3f9c2d1@hookd.example.com  ", "a3f9c2d1", "", true},
		{"trailing dot on the domain", "a3f9c2d1@hookd.example.com.", "a3f9c2d1", "", true},

		// Every in-domain address is accepted, hook or no hook: uniform 250 is
		// what keeps the server from being an enumeration oracle.
		{"unknown local part", "nosuchhook@hookd.example.com", "nosuchhook", "", true},
		{"empty local part", "@hookd.example.com", "", "", true},

		{"subdomain fallback", "anything@a3f9c2d1.hookd.example.com", "a3f9c2d1", "", true},
		{"subdomain fallback with tag", "any+tag@a3f9c2d1.hookd.example.com", "a3f9c2d1", "tag", true},
		{"deep subdomain", "any@x.a3f9c2d1.hookd.example.com", "x", "", true},

		{"source route", "<@relay.example.net:a3f9c2d1@hookd.example.com>", "a3f9c2d1", "", true},
		{"source route without a colon", "<@relay.example.net>", "", "", false},

		{"postmaster without a domain", "<postmaster>", "postmaster", "", true},

		{"foreign domain", "victim@example.org", "", "", false},
		{"domain that merely ends the same", "x@nothookd.example.com", "", "", false},
		{"apex domain itself as local part", "hookd.example.com", "", "", false},
		{"missing at sign", "a3f9c2d1", "", "", false},
		{"null sender", "<>", "", "", false},
		{"empty", "", "", "", false},

		// Only the last '@' separates local part from domain (RFC 5321 4.1.2
		// allows a quoted local part to contain one).
		{"multiple at signs", `"a@b"@hookd.example.com`, `"a@b"`, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, tag, ok := hookIDFromRecipient(tt.addr, domain)
			if ok != tt.wantOK {
				t.Fatalf("hookIDFromRecipient(%q) ok = %v, want %v", tt.addr, ok, tt.wantOK)
			}
			if id != tt.wantID {
				t.Errorf("hookIDFromRecipient(%q) id = %q, want %q", tt.addr, id, tt.wantID)
			}
			if tag != tt.wantTag {
				t.Errorf("hookIDFromRecipient(%q) tag = %q, want %q", tt.addr, tag, tt.wantTag)
			}
		})
	}
}

func TestSplitPath(t *testing.T) {
	tests := []struct {
		name       string
		arg        string
		wantAddr   string
		wantParams string
		wantOK     bool
	}{
		{"plain", "<a@b.test>", "a@b.test", "", true},
		{"leading space", " <a@b.test>", "a@b.test", "", true},
		{"null sender", "<>", "", "", true},
		{"with params", "<a@b.test> SIZE=1234 BODY=8BITMIME", "a@b.test", "SIZE=1234 BODY=8BITMIME", true},
		{"unterminated", "<a@b.test", "", "", false},
		{"no brackets", "a@b.test", "a@b.test", "", true},
		{"no brackets with params", "a@b.test SIZE=10", "a@b.test", "SIZE=10", true},
		{"empty", "", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, params, ok := splitPath(tt.arg)
			if ok != tt.wantOK || addr != tt.wantAddr || params != tt.wantParams {
				t.Errorf("splitPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.arg, addr, params, ok, tt.wantAddr, tt.wantParams, tt.wantOK)
			}
		})
	}
}

func TestSizeParam(t *testing.T) {
	tests := []struct {
		name      string
		params    string
		wantSize  int
		wantGiven bool
	}{
		{"absent", "BODY=8BITMIME", 0, false},
		{"present", "SIZE=4096", 4096, true},
		{"lowercase", "size=4096", 4096, true},
		{"among others", "BODY=8BITMIME SIZE=10 RET=HDRS", 10, true},
		{"unparseable", "SIZE=lots", 0, false},
		{"empty", "", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, given := sizeParam(tt.params)
			if size != tt.wantSize || given != tt.wantGiven {
				t.Errorf("sizeParam(%q) = (%d, %v), want (%d, %v)",
					tt.params, size, given, tt.wantSize, tt.wantGiven)
			}
		})
	}
}

func TestParseSubject(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			"plain",
			"From: a@b.test\r\nSubject: Verify your account\r\n\r\ncode 1234\r\n",
			"Verify your account",
		},
		{
			"rfc 2047 encoded-word",
			"Subject: =?UTF-8?B?VsOpcmlmaWNhdGlvbg==?=\r\n\r\nbody\r\n",
			"Vérification",
		},
		{
			"no subject header",
			"From: a@b.test\r\n\r\nbody\r\n",
			"",
		},
		{
			// A message that does not parse still gets captured; only the
			// subject is lost.
			"unparseable",
			"this is not a message",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseSubject(tt.raw); got != tt.want {
				t.Errorf("parseSubject() = %q, want %q", got, tt.want)
			}
		})
	}
}
