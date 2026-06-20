package commentbus

import "testing"

func TestGenerateLocalID(t *testing.T) {
	cases := []struct {
		kind string
		re   string
	}{
		{"msg", LocalMessageIDRE.String()},
		{"evt", LocalEventIDRE.String()},
		{"op", LocalOperationIDRE.String()},
		{"sess", LocalSessionIDRE.String()},
		{"gen", LocalSessionGenerationIDRE.String()},
	}
	for _, tc := range cases {
		id, err := GenerateLocalID(tc.kind, 0)
		if err != nil {
			t.Fatalf("GenerateLocalID(%s): %v", tc.kind, err)
		}
		if err := ValidateLocalID(tc.kind, id); err != nil {
			t.Fatalf("ValidateLocalID(%s, %s): %v", tc.kind, id, err)
		}
	}
}

func TestValidateLocalIDRejectsUnsafeValues(t *testing.T) {
	if err := ValidateLocalID("msg", "clm_bad;echo owned"); err == nil {
		t.Fatal("expected invalid message id to fail")
	}
	if _, err := GenerateLocalID("op", 8); err == nil {
		t.Fatal("expected short op id generation to fail")
	}
}
