package main

// Flags for chapter languages, as MangaDex shows them. A language isn't a
// country, so these follow MangaDex's own picks, which people already know
// from there. Languages without a fitting flag get none.
var langFlags = map[string]string{
	"en": "GB", "ja": "JP", "ja-ro": "JP", "ko": "KR", "ko-ro": "KR", "zh": "CN", "zh-hk": "HK", "zh-ro": "CN",
	"es": "ES", "es-la": "MX", "fr": "FR", "de": "DE", "it": "IT", "pt": "PT", "pt-br": "BR", "ru": "RU",
	"uk": "UA", "pl": "PL", "tr": "TR", "vi": "VN", "id": "ID", "th": "TH", "ar": "SA", "fa": "IR", "he": "IL",
	"hu": "HU", "cs": "CZ", "ro": "RO", "nl": "NL", "sv": "SE", "fi": "FI", "no": "NO", "da": "DK", "el": "GR",
	"bg": "BG", "ca": "AD", "ms": "MY", "tl": "PH", "hi": "IN", "bn": "BD", "mn": "MN", "my": "MM", "lt": "LT",
	"sr": "RS", "hr": "HR", "ta": "LK", "sk": "SK", "et": "EE", "kk": "KZ", "uz": "UZ", "ka": "GE", "az": "AZ",
	"be": "BY", "ne": "NP", "ur": "PK", "la": "VA", "jv": "ID", "sl": "SI", "lv": "LV",
}

// langFlag returns the flag emoji for a language code, or "".
func langFlag(code string) string {
	cc, ok := langFlags[code]
	if !ok || len(cc) != 2 || cc[0] < 'A' || cc[0] > 'Z' || cc[1] < 'A' || cc[1] > 'Z' {
		return ""
	}
	// Two regional indicator symbols make a flag.
	return string(rune(0x1F1E6+rune(cc[0]-'A'))) + string(rune(0x1F1E6+rune(cc[1]-'A')))
}
