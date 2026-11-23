package extract

// CleanRules is the single place to extend the HTML junk selectors. Token
// matching is done on whole hyphen/underscore-separated tokens, so a class like
// "header-title" is not dropped just because it contains "ad" or "banner".
type CleanRules struct {
	// RemoveTags are structural clutter removed unconditionally.
	RemoveTags []string
	// RemoveRoles are ARIA roles that are never article content.
	RemoveRoles []string
	// WeakTokens only match outside <article>/<main> (a hero banner inside an
	// article may be legitimate content).
	WeakTokens []string
	// StrongTokens match everywhere, including inside <article>/<main>.
	StrongTokens []string
	// ExactJunk are exact (case-insensitive) junk lines.
	ExactJunk []string
	// CTA are case-insensitive regular expressions for call-to-action lines.
	CTA []string
}

// DefaultCleanRules is used unless a caller overrides it (CTA phrases can be
// replaced by CLEAN_CTA_PHRASES_FILE).
var DefaultCleanRules = CleanRules{
	RemoveTags: []string{
		"script", "style", "noscript", "svg", "picture", "iframe", "form", "nav",
		"footer", "aside", "dialog", "template",
	},
	RemoveRoles: []string{"banner", "navigation", "complementary", "contentinfo"},
	WeakTokens: []string{
		"cookie", "consent", "banner", "promo", "newsletter", "subscribe", "cta",
		"sidebar", "related", "share", "social", "advert", "ad", "popup", "modal",
		"breadcrumb",
	},
	StrongTokens: []string{"cookie", "consent", "advert", "ad", "popup", "modal", "newsletter", "subscribe"},
	ExactJunk: []string{
		"skip to main content", "skip to content", "table of contents",
	},
	CTA: []string{
		`install in`, `try .* for free`, `sign up`, `subscribe`, `get started`,
		`check out our`, `learn more`, `book a demo`, `read more`, `download now`,
		`start your`,
	},
}
