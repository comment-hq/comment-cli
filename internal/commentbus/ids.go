package commentbus

import (
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
)

const (
	BusProtocolVersion                    = 1
	FeatureBotletsSetupOrientation        = "botlets_setup_orientation"
	FeatureBotletsSetupOrientationVersion = "multiline-v1"
	// Daemon-mediated agent enrollment feature bits. Defined ahead of the
	// subsystems shipping; not advertised in the daemon health response until
	// pairing (Phase 2) and enrollment (Phase 3) actually exist.
	FeatureDaemonPairing          = "daemon_pairing"
	FeatureDaemonPairingVersion   = 1
	FeatureAgentEnrollment        = "agent_enrollment"
	FeatureAgentEnrollmentVersion = 1
)

var (
	LocalMessageIDRE           = regexp.MustCompile(`^msg_[A-Za-z0-9_-]{20,64}$`)
	LocalEventIDRE             = regexp.MustCompile(`^evt_[A-Za-z0-9_-]{20,64}$`)
	LocalOperationIDRE         = regexp.MustCompile(`^op_[A-Za-z0-9_-]{20,64}$`)
	LocalSessionIDRE           = regexp.MustCompile(`^sess_[A-Za-z0-9_-]{20,64}$`)
	LocalSessionGenerationIDRE = regexp.MustCompile(`^gen_[A-Za-z0-9_-]{16,64}$`)
	BotNameRE                  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	ProfileRE                  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]\.[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
	DocSlugRE                  = regexp.MustCompile(`^[a-z0-9]{3,64}$`)
	UUIDRE                     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	UUIDLikeRE                 = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

const randomAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

func GenerateLocalID(kind string, length int) (string, error) {
	prefix, minLength, err := idPrefix(kind)
	if err != nil {
		return "", err
	}
	if length == 0 {
		if kind == "gen" {
			length = 24
		} else {
			length = 28
		}
	}
	if length < minLength || length > 64 {
		return "", fmt.Errorf("invalid %s id length", kind)
	}
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	out := make([]byte, 0, len(prefix)+length)
	out = append(out, prefix...)
	for _, b := range bytes {
		out = append(out, randomAlphabet[int(b)%len(randomAlphabet)])
	}
	id := string(out)
	if err := ValidateLocalID(kind, id); err != nil {
		return "", err
	}
	return id, nil
}

func GenerateUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4],
		b[4:6],
		b[6:8],
		b[8:10],
		b[10:16],
	)
	if !UUIDRE.MatchString(id) {
		return "", errors.New("generated invalid uuid")
	}
	return id, nil
}

func ValidateLocalID(kind, value string) error {
	var ok bool
	switch kind {
	case "msg":
		ok = LocalMessageIDRE.MatchString(value)
	case "evt":
		ok = LocalEventIDRE.MatchString(value)
	case "op":
		ok = LocalOperationIDRE.MatchString(value)
	case "sess":
		ok = LocalSessionIDRE.MatchString(value)
	case "gen":
		ok = LocalSessionGenerationIDRE.MatchString(value)
	default:
		return fmt.Errorf("unknown id kind %q", kind)
	}
	if !ok {
		return fmt.Errorf("invalid %s id", kind)
	}
	return nil
}

func idPrefix(kind string) (string, int, error) {
	switch kind {
	case "msg":
		return "msg_", 20, nil
	case "evt":
		return "evt_", 20, nil
	case "op":
		return "op_", 20, nil
	case "sess":
		return "sess_", 20, nil
	case "gen":
		return "gen_", 16, nil
	default:
		return "", 0, errors.New("unknown id kind")
	}
}
