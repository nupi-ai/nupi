// Package language provides a static registry of ISO 639-1 languages with
// multiple format representations (ISO 639-1, BCP 47, English name, native name).
// The registry is used by the daemon to validate client language selections and
// expand them into the four standardised metadata keys propagated to adapters.
package language

import "strings"

// Language holds a single language in four standardised formats.
type Language struct {
	ISO1        string // ISO 639-1 two-letter code, e.g. "pl"
	BCP47       string // BCP 47 / IETF language tag, e.g. "pl-PL"
	EnglishName string // English name, e.g. "Polish"
	NativeName  string // Native name, e.g. "Polski"
}

// byISO1 is the map-based index for O(1) lookups by ISO 639-1 code.
var byISO1 map[string]Language

// all is the pre-built sorted slice returned by All().
var all []Language

func init() {
	entries := registryEntries()
	byISO1 = make(map[string]Language, len(entries))
	all = make([]Language, len(entries))
	for i, e := range entries {
		byISO1[e.ISO1] = e
		all[i] = e
	}
}

// Lookup returns the language record for the given ISO 639-1 code.
// The lookup is case-insensitive: "PL", "pl", and "Pl" all match.
// The second return value is false if the code is not in the registry.
func Lookup(iso1 string) (Language, bool) {
	l, ok := byISO1[strings.ToLower(iso1)]
	return l, ok
}

// All returns a copy of the complete language list.
func All() []Language {
	out := make([]Language, len(all))
	copy(out, all)
	return out
}

// registryEntries returns the full list of ISO 639-1 languages.
// Each entry maps an ISO 639-1 code to its BCP 47 tag, English name, and native name.
func registryEntries() []Language {
	return []Language{
		{"aa", "aa-ET", "Afar", "Afaraf"},
		{"ab", "ab-GE", "Abkhazian", "Аԥсуа"},
		{"af", "af-ZA", "Afrikaans", "Afrikaans"},
		{"ak", "ak-GH", "Akan", "Akan"},
		{"am", "am-ET", "Amharic", "አማርኛ"},
		{"an", "an-ES", "Aragonese", "Aragonés"},
		{"ar", "ar-SA", "Arabic", "العربية"},
		{"as", "as-IN", "Assamese", "অসমীয়া"},
		{"av", "av-RU", "Avaric", "Авар"},
		{"ay", "ay-BO", "Aymara", "Aymar"},
		{"az", "az-AZ", "Azerbaijani", "Azərbaycan"},
		{"ba", "ba-RU", "Bashkir", "Башҡорт"},
		{"be", "be-BY", "Belarusian", "Беларуская"},
		{"bg", "bg-BG", "Bulgarian", "Български"},
		{"bh", "bh-IN", "Bihari", "भोजपुरी"},
		{"bi", "bi-VU", "Bislama", "Bislama"},
		{"bm", "bm-ML", "Bambara", "Bamanankan"},
		{"bn", "bn-BD", "Bengali", "বাংলা"},
		{"bo", "bo-CN", "Tibetan", "བོད་སྐད"},
		{"br", "br-FR", "Breton", "Brezhoneg"},
		{"bs", "bs-BA", "Bosnian", "Bosanski"},
		{"ca", "ca-ES", "Catalan", "Català"},
		{"ce", "ce-RU", "Chechen", "Нохчийн"},
		{"ch", "ch-GU", "Chamorro", "Chamoru"},
		{"co", "co-FR", "Corsican", "Corsu"},
		{"cr", "cr-CA", "Cree", "ᓀᐦᐃᔭᐍᐏᐣ"},
		{"cs", "cs-CZ", "Czech", "Čeština"},
		{"cu", "cu-RU", "Church Slavic", "Словѣньскъ"},
		{"cv", "cv-RU", "Chuvash", "Чӑваш"},
		{"cy", "cy-GB", "Welsh", "Cymraeg"},
		{"da", "da-DK", "Danish", "Dansk"},
		{"de", "de-DE", "German", "Deutsch"},
		{"dv", "dv-MV", "Divehi", "ދިވެހި"},
		{"dz", "dz-BT", "Dzongkha", "རྫོང་ཁ"},
		{"ee", "ee-GH", "Ewe", "Eʋegbe"},
		{"el", "el-GR", "Greek", "Ελληνικά"},
		{"en", "en-US", "English", "English"},
		{"eo", "eo-001", "Esperanto", "Esperanto"},
		{"es", "es-ES", "Spanish", "Español"},
		{"et", "et-EE", "Estonian", "Eesti"},
		{"eu", "eu-ES", "Basque", "Euskara"},
		{"fa", "fa-IR", "Persian", "فارسی"},
		{"ff", "ff-SN", "Fulah", "Fulfulde"},
		{"fi", "fi-FI", "Finnish", "Suomi"},
		{"fj", "fj-FJ", "Fijian", "Vosa Vakaviti"},
		{"fo", "fo-FO", "Faroese", "Føroyskt"},
		{"fr", "fr-FR", "French", "Français"},
		{"fy", "fy-NL", "Western Frisian", "Frysk"},
		{"ga", "ga-IE", "Irish", "Gaeilge"},
		{"gd", "gd-GB", "Scottish Gaelic", "Gàidhlig"},
		{"gl", "gl-ES", "Galician", "Galego"},
		{"gn", "gn-PY", "Guarani", "Avañe'ẽ"},
		{"gu", "gu-IN", "Gujarati", "ગુજરાતી"},
		{"gv", "gv-IM", "Manx", "Gaelg"},
		{"ha", "ha-NG", "Hausa", "Hausa"},
		{"he", "he-IL", "Hebrew", "עברית"},
		{"hi", "hi-IN", "Hindi", "हिन्दी"},
		{"ho", "ho-PG", "Hiri Motu", "Hiri Motu"},
		{"hr", "hr-HR", "Croatian", "Hrvatski"},
		{"ht", "ht-HT", "Haitian Creole", "Kreyòl Ayisyen"},
		{"hu", "hu-HU", "Hungarian", "Magyar"},
		{"hy", "hy-AM", "Armenian", "Հայերեն"},
		{"hz", "hz-NA", "Herero", "Otjiherero"},
		{"ia", "ia-001", "Interlingua", "Interlingua"},
		{"id", "id-ID", "Indonesian", "Bahasa Indonesia"},
		{"ie", "ie-001", "Interlingue", "Interlingue"},
		{"ig", "ig-NG", "Igbo", "Igbo"},
		{"ii", "ii-CN", "Sichuan Yi", "ꆈꌠꉙ"},
		{"ik", "ik-US", "Inupiaq", "Iñupiatun"},
		{"io", "io-001", "Ido", "Ido"},
		{"is", "is-IS", "Icelandic", "Íslenska"},
		{"it", "it-IT", "Italian", "Italiano"},
		{"iu", "iu-CA", "Inuktitut", "ᐃᓄᒃᑎᑐᑦ"},
		{"ja", "ja-JP", "Japanese", "日本語"},
		{"jv", "jv-ID", "Javanese", "Basa Jawa"},
		{"ka", "ka-GE", "Georgian", "ქართული"},
		{"kg", "kg-CD", "Kongo", "Kikongo"},
		{"ki", "ki-KE", "Kikuyu", "Gĩkũyũ"},
		{"kj", "kj-NA", "Kuanyama", "Kuanyama"},
		{"kk", "kk-KZ", "Kazakh", "Қазақ"},
		{"kl", "kl-GL", "Kalaallisut", "Kalaallisut"},
		{"km", "km-KH", "Khmer", "ភាសាខ្មែរ"},
		{"kn", "kn-IN", "Kannada", "ಕನ್ನಡ"},
		{"ko", "ko-KR", "Korean", "한국어"},
		{"kr", "kr-NG", "Kanuri", "Kanuri"},
		{"ks", "ks-IN", "Kashmiri", "कश्मीरी"},
		{"ku", "ku-TR", "Kurdish", "Kurdî"},
		{"kv", "kv-RU", "Komi", "Коми"},
		{"kw", "kw-GB", "Cornish", "Kernewek"},
		{"ky", "ky-KG", "Kyrgyz", "Кыргызча"},
		{"la", "la-VA", "Latin", "Latina"},
		{"lb", "lb-LU", "Luxembourgish", "Lëtzebuergesch"},
		{"lg", "lg-UG", "Ganda", "Luganda"},
		{"li", "li-NL", "Limburgish", "Limburgs"},
		{"ln", "ln-CD", "Lingala", "Lingála"},
		{"lo", "lo-LA", "Lao", "ພາສາລາວ"},
		{"lt", "lt-LT", "Lithuanian", "Lietuvių"},
		{"lu", "lu-CD", "Luba-Katanga", "Tshiluba"},
		{"lv", "lv-LV", "Latvian", "Latviešu"},
		{"mg", "mg-MG", "Malagasy", "Malagasy"},
		{"mh", "mh-MH", "Marshallese", "Kajin M̧ajeļ"},
		{"mi", "mi-NZ", "Maori", "Te Reo Māori"},
		{"mk", "mk-MK", "Macedonian", "Македонски"},
		{"ml", "ml-IN", "Malayalam", "മലയാളം"},
		{"mn", "mn-MN", "Mongolian", "Монгол"},
		{"mr", "mr-IN", "Marathi", "मराठी"},
		{"ms", "ms-MY", "Malay", "Bahasa Melayu"},
		{"mt", "mt-MT", "Maltese", "Malti"},
		{"my", "my-MM", "Burmese", "မြန်မာစာ"},
		{"na", "na-NR", "Nauru", "Dorerin Naoero"},
		{"nb", "nb-NO", "Norwegian Bokmål", "Norsk Bokmål"},
		{"nd", "nd-ZW", "North Ndebele", "isiNdebele"},
		{"ne", "ne-NP", "Nepali", "नेपाली"},
		{"ng", "ng-NA", "Ndonga", "Owambo"},
		{"nl", "nl-NL", "Dutch", "Nederlands"},
		{"nn", "nn-NO", "Norwegian Nynorsk", "Norsk Nynorsk"},
		{"no", "no-NO", "Norwegian", "Norsk"},
		{"nr", "nr-ZA", "South Ndebele", "isiNdebele"},
		{"nv", "nv-US", "Navajo", "Diné Bizaad"},
		{"ny", "ny-MW", "Chichewa", "Chichewa"},
		{"oc", "oc-FR", "Occitan", "Occitan"},
		{"oj", "oj-CA", "Ojibwa", "ᐊᓂᔑᓈᐯᒧᐎᓐ"},
		{"om", "om-ET", "Oromo", "Afaan Oromoo"},
		{"or", "or-IN", "Odia", "ଓଡ଼ିଆ"},
		{"os", "os-GE", "Ossetian", "Ирон"},
		{"pa", "pa-IN", "Punjabi", "ਪੰਜਾਬੀ"},
		{"pi", "pi-IN", "Pali", "पालि"},
		{"pl", "pl-PL", "Polish", "Polski"},
		{"ps", "ps-AF", "Pashto", "پښتو"},
		{"pt", "pt-PT", "Portuguese", "Português"},
		{"qu", "qu-PE", "Quechua", "Runa Simi"},
		{"rm", "rm-CH", "Romansh", "Rumantsch"},
		{"rn", "rn-BI", "Rundi", "Ikirundi"},
		{"ro", "ro-RO", "Romanian", "Română"},
		{"ru", "ru-RU", "Russian", "Русский"},
		{"rw", "rw-RW", "Kinyarwanda", "Ikinyarwanda"},
		{"sa", "sa-IN", "Sanskrit", "संस्कृतम्"},
		{"sc", "sc-IT", "Sardinian", "Sardu"},
		{"sd", "sd-PK", "Sindhi", "سنڌي"},
		{"se", "se-NO", "Northern Sami", "Davvisámegiella"},
		{"sg", "sg-CF", "Sango", "Sängö"},
		{"si", "si-LK", "Sinhala", "සිංහල"},
		{"sk", "sk-SK", "Slovak", "Slovenčina"},
		{"sl", "sl-SI", "Slovenian", "Slovenščina"},
		{"sm", "sm-WS", "Samoan", "Gagana Samoa"},
		{"sn", "sn-ZW", "Shona", "ChiShona"},
		{"so", "so-SO", "Somali", "Soomaali"},
		{"sq", "sq-AL", "Albanian", "Shqip"},
		{"sr", "sr-RS", "Serbian", "Српски"},
		{"ss", "ss-SZ", "Swati", "SiSwati"},
		{"st", "st-ZA", "Southern Sotho", "Sesotho"},
		{"su", "su-ID", "Sundanese", "Basa Sunda"},
		{"sv", "sv-SE", "Swedish", "Svenska"},
		{"sw", "sw-TZ", "Swahili", "Kiswahili"},
		{"ta", "ta-IN", "Tamil", "தமிழ்"},
		{"te", "te-IN", "Telugu", "తెలుగు"},
		{"tg", "tg-TJ", "Tajik", "Тоҷикӣ"},
		{"th", "th-TH", "Thai", "ไทย"},
		{"ti", "ti-ER", "Tigrinya", "ትግርኛ"},
		{"tk", "tk-TM", "Turkmen", "Türkmen"},
		{"tl", "tl-PH", "Tagalog", "Tagalog"},
		{"tn", "tn-BW", "Tswana", "Setswana"},
		{"to", "to-TO", "Tonga", "Lea Faka-Tonga"},
		{"tr", "tr-TR", "Turkish", "Türkçe"},
		{"ts", "ts-ZA", "Tsonga", "Xitsonga"},
		{"tt", "tt-RU", "Tatar", "Татар"},
		{"tw", "tw-GH", "Twi", "Twi"},
		{"ty", "ty-PF", "Tahitian", "Reo Tahiti"},
		{"ug", "ug-CN", "Uyghur", "ئۇيغۇرچە"},
		{"uk", "uk-UA", "Ukrainian", "Українська"},
		{"ur", "ur-PK", "Urdu", "اردو"},
		{"uz", "uz-UZ", "Uzbek", "Oʻzbek"},
		{"ve", "ve-ZA", "Venda", "Tshivenḓa"},
		{"vi", "vi-VN", "Vietnamese", "Tiếng Việt"},
		{"vo", "vo-001", "Volapük", "Volapük"},
		{"wa", "wa-BE", "Walloon", "Walon"},
		{"wo", "wo-SN", "Wolof", "Wollof"},
		{"xh", "xh-ZA", "Xhosa", "isiXhosa"},
		{"yi", "yi-001", "Yiddish", "ייִדיש"},
		{"yo", "yo-NG", "Yoruba", "Yorùbá"},
		{"za", "za-CN", "Zhuang", "Saɯ Cueŋƅ"},
		{"zh", "zh-CN", "Chinese", "中文"},
		{"zu", "zu-ZA", "Zulu", "isiZulu"},
	}
}
