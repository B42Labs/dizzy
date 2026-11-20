package horizon

import (
	"errors"
	"fmt"
	"io"

	"golang.org/x/net/html"
)

// parseLoginForm reads Horizon's login page and returns the value of its
// csrfmiddlewaretoken input and its region: the value of the region input,
// which Horizon renders hidden when one region is configured, or of the
// region select, its selected option and otherwise its first. A page
// without a non-empty token, or without a region control with a value,
// returns ErrMismatch. A page that cannot be tokenized returns the
// tokenizer's error.
func parseLoginForm(r io.Reader) (csrfToken, region string, err error) {
	var (
		haveRegion     bool
		inRegionSelect bool
		firstOption    string
		haveFirst      bool
		selected       string
		haveSelected   bool
	)
	z := html.NewTokenizer(r)
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if !errors.Is(z.Err(), io.EOF) {
				return "", "", fmt.Errorf("reading the login form: %w", z.Err())
			}
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			switch tok.Data {
			case "input":
				switch attr(tok, "name") {
				case "csrfmiddlewaretoken":
					if csrfToken == "" {
						csrfToken = attr(tok, "value")
					}
				case "region":
					if !haveRegion {
						region, haveRegion = attr(tok, "value"), true
					}
				}
			case "select":
				if attr(tok, "name") == "region" && !haveRegion {
					inRegionSelect = true
				}
			case "option":
				if !inRegionSelect {
					break
				}
				v := attr(tok, "value")
				if !haveFirst {
					firstOption, haveFirst = v, true
				}
				if hasAttr(tok, "selected") && !haveSelected {
					selected, haveSelected = v, true
				}
			}
		case html.EndTagToken:
			if inRegionSelect && z.Token().Data == "select" {
				inRegionSelect = false
				switch {
				case haveSelected:
					region, haveRegion = selected, true
				case haveFirst:
					region, haveRegion = firstOption, true
				}
			}
		}
	}

	if csrfToken == "" {
		return "", "", fmt.Errorf("%w: the login page has no csrfmiddlewaretoken", ErrMismatch)
	}
	if !haveRegion || region == "" {
		return "", "", fmt.Errorf("%w: the login page has no region", ErrMismatch)
	}
	return csrfToken, region, nil
}

// attr returns the value of the attribute key of tok, or "" when it has
// none.
func attr(tok html.Token, key string) string {
	for _, a := range tok.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hasAttr reports whether tok carries the attribute key.
func hasAttr(tok html.Token, key string) bool {
	for _, a := range tok.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}
