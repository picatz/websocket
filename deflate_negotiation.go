package websocket

import (
	"fmt"
	"strconv"
	"strings"
)

// A zero window is absent (effective 15); -1 is the bare client offer parameter.
// Preserve presence: an omitted response cannot satisfy a server window offer,
// even when both have an effective value of 15.
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

func (p pmdParameters) validateLocalWindow(server bool) error {
	bits, name := p.clientMaxWindowBits, "client_max_window_bits"
	if server {
		bits, name = p.serverMaxWindowBits, "server_max_window_bits"
	}
	// compress/flate's normal encoder has a fixed 32 KiB window. Compression
	// level is a separate control and cannot enforce a smaller window.
	if bits != 0 && bits != 15 {
		return fmt.Errorf("%w: local compressor requires %s=15 (got %d)", ErrInvalidExtension, name, bits)
	}
	return nil
}

func validatePMDConfiguration(extensions []Extension, server bool) error {
	for _, ext := range extensions {
		if pmd, ok := ext.(*perMessageDeflate); ok {
			if err := pmd.configured.validateLocalWindow(server); err != nil {
				return err
			}
		}
	}
	return nil
}

func (pmd *perMessageDeflate) Name() string { return "permessage-deflate" }

func (pmd *perMessageDeflate) Offer() string {
	if pmd.enabled {
		return pmd.negotiated.String()
	}
	// Dial calls Offer before touching the transport. Keep exactly what was
	// offered for later validation, rather than reconstructing it from the
	// selected response or changing configuration during negotiation.
	pmd.offered, pmd.offerSet = pmd.configured, true
	return pmd.offered.String()
}

// Negotiate interprets a server response, as required by direct client use.
// Upgrade uses the offer-side path below instead.
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
	return pmd.negotiate(extensions, false)
}

func (pmd *perMessageDeflate) negotiate(extensions []extensionOffer, server bool) error {
	pmd.enabled, pmd.negotiated, pmd.server = false, pmdParameters{}, server
	if err := pmd.configured.validateLocalWindow(server); err != nil {
		return err
	}
	if !pmd.offerSet {
		pmd.offered, pmd.offerSet = pmd.configured, true
	}
	var selected bool
	for _, ext := range extensions {
		if ext.name == pmd.Name() {
			if selected && !server {
				return fmt.Errorf("%w: repeated permessage-deflate selection", ErrInvalidExtension)
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
				if err == nil {
					err = params.validateLocalWindow(false)
				}
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
			return pmdParameters{}, fmt.Errorf("%w: repeated permessage-deflate parameter %q", ErrInvalidExtension, param.name)
		}
		seen[param.name] = true
		switch param.name {
		case "client_no_context_takeover", "server_no_context_takeover":
			if param.hasValue {
				return pmdParameters{}, fmt.Errorf("%w: permessage-deflate flag %q has a value", ErrInvalidExtension, param.name)
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
					return pmdParameters{}, fmt.Errorf("%w: invalid permessage-deflate parameter %q", ErrInvalidExtension, param.name)
				}
			}
			if param.name == "client_max_window_bits" {
				p.clientMaxWindowBits = bits
			} else {
				p.serverMaxWindowBits = bits
			}
		default:
			return pmdParameters{}, fmt.Errorf("%w: unknown permessage-deflate parameter %q", ErrInvalidExtension, param.name)
		}
	}
	return p, nil
}

// A client window value and client no-context flag in an offer are hints,
// whereas server window and server no-context parameters are constraints.
// RFC 7692 sections 7.1.1, 7.1.2 and 7.2.1 deliberately make these asymmetric.
func validatePMDResponse(response, offer pmdParameters) error {
	if response.clientMaxWindowBits != 0 && offer.clientMaxWindowBits == 0 {
		return fmt.Errorf("%w: unoffered client_max_window_bits", ErrInvalidExtension)
	}
	if offer.serverMaxWindowBits != 0 && (response.serverMaxWindowBits == 0 || response.serverMaxWindowBits > offer.serverMaxWindowBits) {
		return fmt.Errorf("%w: missing or exceeded server_max_window_bits constraint", ErrInvalidExtension)
	}
	if offer.serverNoContextTakeover && !response.serverNoContextTakeover {
		return fmt.Errorf("%w: missing server_no_context_takeover constraint", ErrInvalidExtension)
	}
	return nil
}

func (pmd *perMessageDeflate) selectParameters(offer pmdParameters) (pmdParameters, error) {
	if err := offer.validateLocalWindow(true); err != nil {
		return pmdParameters{}, err
	}
	p := pmd.configured
	p.clientNoContextTakeover = p.clientNoContextTakeover || offer.clientNoContextTakeover
	p.serverNoContextTakeover = p.serverNoContextTakeover || offer.serverNoContextTakeover
	if offer.serverMaxWindowBits != 0 {
		p.serverMaxWindowBits = 15
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
			return pmdParameters{}, fmt.Errorf("%w: client_max_window_bits constraint was not offered", ErrInvalidExtension)
		}
		p.clientMaxWindowBits = 0
	}
	return p, validatePMDResponse(p, offer)
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
