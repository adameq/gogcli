package cmd

import (
	"context"
	"strings"

	"google.golang.org/api/gmail/v1"

	"github.com/openclaw/gogcli/internal/ui"
)

// plainBlock is the signature as appended to a plain-text body, or "" when the
// signature has no plain text (for example an image-only signature).
func (s composeSignature) plainBlock() string {
	if strings.TrimSpace(s.Plain) == "" {
		return ""
	}
	return "--\n" + strings.TrimSpace(s.Plain)
}

// htmlBlock is the signature as appended to an HTML body, or "" when empty.
func (s composeSignature) htmlBlock() string {
	if strings.TrimSpace(s.HTML) == "" {
		return ""
	}
	return `<div class="gmail_signature">` + strings.TrimSpace(s.HTML) + `</div>`
}

// requestedSignature resolves the signature the signature flags ask for. It
// returns the zero value when none was requested or the resolved one is empty;
// an empty one also warns on stderr, so callers can append the result as-is.
func (c *composeSignatureOptions) requestedSignature(ctx context.Context, svc *gmail.Service, sendingEmail string) (composeSignature, error) {
	if !c.signatureRequested() {
		return composeSignature{}, nil
	}
	signature, source, err := c.resolveComposeSignature(ctx, svc, sendingEmail)
	if err != nil {
		return composeSignature{}, err
	}
	if signature.empty() {
		ui.FromContext(ctx).Err().Linef("Warning: no signature configured for %s", source)
		return composeSignature{}, nil
	}
	return signature, nil
}
