package websocket

import (
	"fmt"
	"net/http"
	"strings"
)

type extensionParameter struct {
	name, value string
	hasValue    bool
}

type extensionOffer struct {
	name   string
	params []extensionParameter
}

// extensionValues includes noncanonical keys in hand-built requests. net/http
// preserves the wire order of repeated fields in the canonical key's slice.
func extensionValues(header http.Header) []string {
	var values []string
	for name, fields := range header {
		if strings.EqualFold(name, "Sec-WebSocket-Extensions") {
			values = append(values, fields...)
		}
	}
	return values
}

// parseExtensions implements RFC 6455 section 9.1, including HTTP's 1# list
// rule, optional whitespace, and quoted values that unescape to tokens.
// Repeated fields are allowed in both directions (RFC 6455 erratum 3433).
// Keep parameters and offer alternatives separate: their semantics belong to
// each extension, and repeated names are not a generic syntax error.
func parseExtensions(values []string) ([]extensionOffer, error) {
	var extensions []extensionOffer
	for _, value := range values {
		s := strings.Trim(value, " \t")
		for s != "" {
			if s[0] == ',' { // Empty list elements do not count toward 1#.
				s = strings.TrimLeft(s[1:], " \t")
				continue
			}
			name, rest := consumeExtensionToken(s)
			if name == "" {
				return nil, fmt.Errorf("%w: expected extension token", ErrInvalidExtension)
			}
			ext := extensionOffer{name: name}
			s = strings.TrimLeft(rest, " \t")
			for s != "" && s[0] == ';' {
				s = strings.TrimLeft(s[1:], " \t")
				name, rest = consumeExtensionToken(s)
				if name == "" {
					return nil, fmt.Errorf("%w: expected parameter token", ErrInvalidExtension)
				}
				param := extensionParameter{name: name}
				s = strings.TrimLeft(rest, " \t")
				if s != "" && s[0] == '=' {
					param.hasValue = true
					s = strings.TrimLeft(s[1:], " \t")
					if s != "" && s[0] == '"' {
						var ok bool
						param.value, s, ok = consumeExtensionQuotedToken(s)
						if !ok {
							return nil, fmt.Errorf("%w: invalid quoted parameter token", ErrInvalidExtension)
						}
					} else {
						param.value, s = consumeExtensionToken(s)
						if param.value == "" {
							return nil, fmt.Errorf("%w: expected parameter value", ErrInvalidExtension)
						}
					}
					s = strings.TrimLeft(s, " \t")
				}
				ext.params = append(ext.params, param)
			}
			if s != "" && s[0] != ',' {
				return nil, fmt.Errorf("%w: expected extension separator", ErrInvalidExtension)
			}
			extensions = append(extensions, ext)
		}
	}
	if len(values) != 0 && len(extensions) == 0 {
		return nil, fmt.Errorf("%w: empty extension list", ErrInvalidExtension)
	}
	return extensions, nil
}

func consumeExtensionToken(s string) (token, rest string) {
	i := 0
	for i < len(s) && isHTTPToken(s[i:i+1]) {
		i++
	}
	return s[:i], s[i:]
}

func consumeExtensionQuotedToken(s string) (token, rest string, ok bool) {
	var value strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] == '"' {
			return value.String(), s[i+1:], value.Len() != 0
		}
		if s[i] == '\\' {
			i++
			if i == len(s) {
				break
			}
		}
		// RFC 6455 restricts the unescaped value to the token alphabet.
		if !isHTTPToken(s[i : i+1]) {
			return "", "", false
		}
		value.WriteByte(s[i])
	}
	return "", "", false
}

// Validate names against actual wire offers rather than substring matches or
// Extension.Name. Custom extensions retain ownership of their parameter and
// repetition semantics through Negotiate's complete-header callback.
func validateExtensionSelection(selected, offered []extensionOffer) error {
	names := make(map[string]bool, len(offered))
	for _, ext := range offered {
		names[ext.name] = true
	}
	selectedPMD := false
	for _, ext := range selected {
		if !names[ext.name] {
			return fmt.Errorf("%w: unoffered extension %q", ErrInvalidExtension, ext.name)
		}
		if ext.name == "permessage-deflate" {
			if selectedPMD {
				return fmt.Errorf("%w: repeated permessage-deflate selection", ErrInvalidExtension)
			}
			selectedPMD = true
		}
	}
	return nil
}

func extensionOffers(extensions []Extension) (string, []extensionOffer, error) {
	var values []string
	for _, ext := range extensions {
		if ext == nil {
			return "", nil, fmt.Errorf("%w: nil extension", ErrInvalidExtension)
		}
		values = append(values, ext.Offer())
	}
	offers, err := parseExtensions(values)
	return strings.Join(values, ", "), offers, err
}
