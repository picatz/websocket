package websocket

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

// A zero window is absent (effective 15); -1 is the bare client offer
// parameter. Preserve presence: an omitted response cannot satisfy a server
// window offer, even when both have an effective value of 15.
type pmdParameters struct {
	clientNoContextTakeover bool
	serverNoContextTakeover bool
	clientMaxWindowBits     int
	serverMaxWindowBits     int
}

func (p pmdParameters) String() string {
	params := []string{"permessage-deflate"}
	if p.clientNoContextTakeover {
		params = append(params, "client_no_context_takeover")
	}
	if p.serverNoContextTakeover {
		params = append(params, "server_no_context_takeover")
	}
	if p.clientMaxWindowBits == -1 {
		params = append(params, "client_max_window_bits")
	} else if p.clientMaxWindowBits > 0 {
		params = append(params, fmt.Sprintf("client_max_window_bits=%d", p.clientMaxWindowBits))
	}
	if p.serverMaxWindowBits > 0 {
		params = append(params, fmt.Sprintf("server_max_window_bits=%d", p.serverMaxWindowBits))
	}
	return strings.Join(params, "; ")
}

func (pmd *perMessageDeflate) Name() string { return "permessage-deflate" }

func (pmd *perMessageDeflate) Offer() string {
	if pmd.enabled {
		return pmd.negotiated.String()
	}
	// Dial calls Offer before touching the transport. Keep exactly what was
	// offered for later validation, rather than reconstructing it from the
	// selected response or changing configuration during negotiation.
	pmd.offered, pmd.offerSet = pmd.clientOffer(), true
	return pmd.offered.String()
}

// Negotiate uses the role established by NewConn. Before attachment it defaults
// to the client response path; manual servers must bind their role first.
func (pmd *perMessageDeflate) Negotiate(response string) error {
	var values []string
	if response != "" {
		values = []string{response}
	}
	extensions, err := parseExtensions(values)
	if err != nil {
		pmd.enabled, pmd.negotiated = false, pmdParameters{}
		return err
	}
	return pmd.negotiate(extensions, pmd.server)
}

func (pmd *perMessageDeflate) negotiate(extensions []extensionOffer, server bool) error {
	pmd.enabled, pmd.negotiated, pmd.server = false, pmdParameters{}, server
	pmd.sendInit = false
	if !pmd.offerSet {
		pmd.offered, pmd.offerSet = pmd.clientOffer(), true
	}
	var selected bool
	for _, ext := range extensions {
		if ext.name == pmd.Name() {
			if selected && !server {
				return fmt.Errorf("%w: repeated permessage-deflate selection", errInvalidExtension)
			}
			selected = true
		}
	}
	for _, ext := range extensions {
		if ext.name != pmd.Name() {
			continue
		}
		params, err := parsePMDParameters(ext.params, server)
		if err == nil {
			if server {
				params, err = pmd.selectParameters(params)
			} else {
				err = validatePMDResponse(params, pmd.offered)
			}
		}
		if err != nil {
			if server {
				continue // Alternatives are independent; never combine their state.
			}
			return err
		}
		pmd.negotiated, pmd.enabled = params, true
		return nil
	}
	return nil
}

func parsePMDParameters(params []extensionParameter, offer bool) (pmdParameters, error) {
	var p pmdParameters
	seen := make(map[string]bool, len(params))
	for _, param := range params {
		if seen[param.name] {
			return pmdParameters{}, fmt.Errorf("%w: repeated permessage-deflate parameter %q", errInvalidExtension, param.name)
		}
		seen[param.name] = true
		switch param.name {
		case "client_no_context_takeover", "server_no_context_takeover":
			if param.hasValue {
				return pmdParameters{}, fmt.Errorf("%w: permessage-deflate flag %q has a value", errInvalidExtension, param.name)
			}
			if param.name == "client_no_context_takeover" {
				p.clientNoContextTakeover = true
			} else {
				p.serverNoContextTakeover = true
			}
		case "client_max_window_bits", "server_max_window_bits":
			bits := -1
			if !(offer && param.name == "client_max_window_bits" && !param.hasValue) {
				var err error
				bits, err = strconv.Atoi(param.value)
				if !param.hasValue || err != nil || bits < 8 || bits > 15 || strconv.Itoa(bits) != param.value {
					return pmdParameters{}, fmt.Errorf("%w: invalid permessage-deflate parameter %q", errInvalidExtension, param.name)
				}
			}
			if param.name == "client_max_window_bits" {
				p.clientMaxWindowBits = bits
			} else {
				p.serverMaxWindowBits = bits
			}
		default:
			return pmdParameters{}, fmt.Errorf("%w: unknown permessage-deflate parameter %q", errInvalidExtension, param.name)
		}
	}
	return p, nil
}

// A client window value and client no-context flag in an offer are hints,
// whereas server window and server no-context parameters are constraints.
// RFC 7692 sections 7.1.1, 7.1.2 and 7.2.1 deliberately make these asymmetric.
func validatePMDResponse(response, offer pmdParameters) error {
	if response.clientMaxWindowBits != 0 && offer.clientMaxWindowBits == 0 {
		return fmt.Errorf("%w: unoffered client_max_window_bits", errInvalidExtension)
	}
	if offer.serverMaxWindowBits != 0 && (response.serverMaxWindowBits == 0 || response.serverMaxWindowBits > offer.serverMaxWindowBits) {
		return fmt.Errorf("%w: missing or exceeded server_max_window_bits constraint", errInvalidExtension)
	}
	if offer.serverNoContextTakeover && !response.serverNoContextTakeover {
		return fmt.Errorf("%w: missing server_no_context_takeover constraint", errInvalidExtension)
	}
	return nil
}

// clientOffer is the configuration a client offers. Without an explicit
// client window it offers a bare client_max_window_bits, declaring support for
// any window the server selects (RFC 7692 section 7.1.2.2).
func (pmd *perMessageDeflate) clientOffer() pmdParameters {
	p := pmd.configured
	if p.clientMaxWindowBits == 0 {
		p.clientMaxWindowBits = -1
	}
	if pmd.memoryLimit > 0 {
		if bits, ok := pmd.budgetWindowBits(); ok {
			p.serverMaxWindowBits = boundWindow(p.serverMaxWindowBits, bits)
			if bits < 15 && (p.clientMaxWindowBits < 0 || bits < p.clientMaxWindowBits) {
				p.clientMaxWindowBits = bits // A bare parameter allows any value.
			}
		} else {
			p.serverNoContextTakeover = true
			p.clientNoContextTakeover = true
		}
	}
	return p
}

// budgetWindowBits is the largest window whose history fits in half the memory
// limit, and false when not even the smallest 2^8-byte window fits.
func (pmd *perMessageDeflate) budgetWindowBits() (int, bool) {
	half := pmd.memoryLimit / 2
	if half < 1<<8 {
		return 0, false
	}
	bits := 15
	for 1<<bits > half {
		bits--
	}
	return bits, true
}

// boundWindow lowers a window parameter to bits. Zero (absent) means 15 and
// stays absent when no bound is needed.
func boundWindow(param, bits int) int {
	if bits >= cmp.Or(param, 15) {
		return param
	}
	return bits
}

func (pmd *perMessageDeflate) selectParameters(offer pmdParameters) (pmdParameters, error) {
	p := pmd.configured
	p.clientNoContextTakeover = p.clientNoContextTakeover || offer.clientNoContextTakeover
	p.serverNoContextTakeover = p.serverNoContextTakeover || offer.serverNoContextTakeover
	// An offered server window is a constraint the response must meet with
	// the same or a smaller value. A configured window may be included even
	// when the client did not offer one (RFC 7692 section 7.1.2.1).
	if offer.serverMaxWindowBits != 0 {
		p.serverMaxWindowBits = min(offer.serverMaxWindowBits, cmp.Or(p.serverMaxWindowBits, 15))
	}
	if pmd.memoryLimit > 0 {
		if bits, ok := pmd.budgetWindowBits(); ok {
			p.serverMaxWindowBits = boundWindow(p.serverMaxWindowBits, bits)
			if offer.clientMaxWindowBits != 0 {
				p.clientMaxWindowBits = boundWindow(p.clientMaxWindowBits, bits)
			} else if bits < 15 && p.clientMaxWindowBits == 0 {
				// The client's window cannot be bounded without its offer
				// (section 7.1.2.2), but its context can always be refused.
				p.clientNoContextTakeover = true
			}
		} else {
			p.serverNoContextTakeover = true
			p.clientNoContextTakeover = true
		}
	}
	if offer.clientMaxWindowBits != 0 {
		if p.clientMaxWindowBits == 0 {
			p.clientMaxWindowBits = 15
		}
		if offer.clientMaxWindowBits > 0 {
			p.clientMaxWindowBits = min(p.clientMaxWindowBits, offer.clientMaxWindowBits)
		}
	} else {
		// A configured peer bound below 15 needs the client's permission to
		// include this parameter. Decline this alternative if it is missing.
		if p.clientMaxWindowBits != 0 && p.clientMaxWindowBits != 15 {
			return pmdParameters{}, fmt.Errorf("%w: client_max_window_bits constraint was not offered", errInvalidExtension)
		}
		p.clientMaxWindowBits = 0
	}
	return p, validatePMDResponse(p, offer)
}

// localWindowBits is the LZ77 window this endpoint compresses with: the
// negotiated limit for its role, or 15 when none was agreed. A client also
// honors the hint in its own offer (RFC 7692 section 7.2.1).
func (pmd *perMessageDeflate) localWindowBits() int {
	if pmd.server {
		return cmp.Or(pmd.negotiated.serverMaxWindowBits, 15)
	}
	bits := 15
	if b := pmd.negotiated.clientMaxWindowBits; b > 0 {
		bits = b
	}
	if b := pmd.offered.clientMaxWindowBits; b > 0 {
		bits = min(bits, b)
	}
	return bits
}

// peerWindowBits is the LZ77 window the peer compresses with: the negotiated
// limit for the peer's role, or 15 when none was agreed.
func (pmd *perMessageDeflate) peerWindowBits() int {
	bits := pmd.negotiated.serverMaxWindowBits
	if pmd.server {
		bits = pmd.negotiated.clientMaxWindowBits
	}
	if bits <= 0 {
		return 15
	}
	return bits
}

func (pmd *perMessageDeflate) localNoContextTakeover() bool {
	if pmd.server {
		return pmd.negotiated.serverNoContextTakeover
	}
	return pmd.negotiated.clientNoContextTakeover
}

func (pmd *perMessageDeflate) peerNoContextTakeover() bool {
	if pmd.server {
		return pmd.negotiated.clientNoContextTakeover
	}
	return pmd.negotiated.serverNoContextTakeover
}
