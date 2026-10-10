// Package export renders deterministic, edit-preserving Obsidian transaction notes.
package export

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/money"
)

const TemplateVersion = 1

var opaqueID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func Hash(block []byte) string { h := sha256.Sum256(block); return hex.EncodeToString(h[:]) }
func ItemHash(itemID string) string {
	h := sha256.Sum256([]byte(itemID))
	return hex.EncodeToString(h[:16])
}

// Text emits provider-controlled punctuation as entities. HTML, Markdown, wikilinks,
// marker comments and bare URLs cannot become active syntax.
func Text(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			b.WriteByte(' ')
			continue
		}
		if r < 128 && (unicode.IsPunct(r) || unicode.IsSymbol(r)) {
			fmt.Fprintf(&b, "&#%d;", r)
			// Some Markdown engines linkify text after decoding entities. Invisible
			// breaks also prevent a decoded provider URL/email from becoming a link.
			if r == ':' || r == '/' || r == '@' || r == '.' {
				b.WriteString("&#8203;")
			}
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func value(v *string) string {
	if v == nil {
		return "unknown"
	}
	return Text(*v)
}

var dateOnly = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func dateValue(v *string) string {
	if v != nil && dateOnly.MatchString(*v) {
		return *v
	}
	return value(v)
}
func Render(n domain.Note) []byte {
	t := n.Transaction
	title := "Transaction"
	if t.MerchantName != nil && *t.MerchantName != "" {
		title = *t.MerchantName
	} else if t.Name != nil && *t.Name != "" {
		title = *t.Name
	}
	status := "posted"
	if t.Pending != nil && *t.Pending {
		status = "pending"
	}
	if t.Removed {
		status = "removed"
	}
	account := Text(n.Account.Name)
	if n.Account.Mask != nil {
		account += " (" + Text(*n.Account.Mask) + ")"
	}
	currency := "unknown currency"
	if t.Currency != nil {
		currency = Text(*t.Currency)
	} else if t.UnofficialCurrency != nil {
		currency = Text(*t.UnofficialCurrency) + " (unofficial)"
	}
	amount := "unknown"
	direction := "outflow"
	if t.Amount != nil {
		amount = *t.Amount
		if strings.HasPrefix(amount, "-") {
			direction = "inflow"
		}
		if amount == "0" {
			direction = "zero"
		}
		amount = money.Display(amount)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- obsidian-fin:begin %s -->\n# %s\n\n", n.ID, Text(title))
	fmt.Fprintf(&b, "- Date: %s\n", dateValue(t.Date))
	if t.AuthorizedDate != nil {
		fmt.Fprintf(&b, "- Authorized date: %s\n", dateValue(t.AuthorizedDate))
	}
	fmt.Fprintf(&b, "- Account: %s\n- Amount: %s %s (%s)\n- Status: %s\n", account, currency, amount, direction, status)
	if t.Name != nil {
		fmt.Fprintf(&b, "- Source name: %s\n", value(t.Name))
	}
	if t.MerchantName != nil {
		fmt.Fprintf(&b, "- Merchant: %s\n", value(t.MerchantName))
	}
	fmt.Fprintf(&b, "- Plaid transaction ID: %s\n", Text(t.ID))
	if t.PendingID != nil {
		fmt.Fprintf(&b, "- Pending predecessor ID: %s\n", value(t.PendingID))
	}
	if t.CategoryPrimary != nil || t.CategoryDetail != nil {
		fmt.Fprintf(&b, "- Plaid category: %s / %s\n", value(t.CategoryPrimary), value(t.CategoryDetail))
	}
	if t.CategoryConfidence != nil {
		fmt.Fprintf(&b, "- Plaid category confidence: %s\n", value(t.CategoryConfidence))
	}
	fmt.Fprintf(&b, "<!-- obsidian-fin:end %s -->", n.ID)
	return []byte(b.String())
}

func Initial(block []byte) []byte {
	prefix := []byte("---\ncategory: \"\"\ntags: []\n---\n")
	result := append(prefix, block...)
	return append(result, []byte("\n\n## Notes\nUser-owned notes go here.\n")...)
}

func BlockBounds(content []byte, noteID string) (int, int, bool) {
	s := string(content)
	if strings.Count(s, "<!-- obsidian-fin:") != 2 {
		return 0, 0, false
	}
	begin := "<!-- obsidian-fin:begin " + noteID + " -->"
	end := "<!-- obsidian-fin:end " + noteID + " -->"
	if strings.Count(s, begin) != 1 || strings.Count(s, end) != 1 {
		return 0, 0, false
	}
	i := strings.Index(s, begin)
	j := strings.Index(s, end) + len(end)
	if i < 0 || j <= i+len(begin) {
		return 0, 0, false
	}
	if i > 0 && s[i-1] != '\n' {
		return 0, 0, false
	}
	if i+len(begin) >= len(s) || s[i+len(begin)] != '\n' {
		return 0, 0, false
	}
	if s[j-len(end)-1] != '\n' {
		return 0, 0, false
	}
	if j < len(s) && s[j] != '\n' && !(s[j] == '\r' && j+1 < len(s) && s[j+1] == '\n') {
		return 0, 0, false
	}
	return i, j, true
}
