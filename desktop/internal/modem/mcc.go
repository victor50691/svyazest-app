package modem

// MCC (ITU-T E.212 mobile country code) -> ISO 3166-1 alpha-2, lower case.
// CIS and Europe complete, the rest -- the countries partners are likely
// to roam in. Unknown MCC -> "".

var mccCountry = map[string]string{
	// CIS / former USSR
	"250": "ru", "255": "ua", "257": "by", "259": "md", "282": "ge", "283": "am",
	"400": "az", "401": "kz", "434": "uz", "436": "tj", "437": "kg", "438": "tm",
	"246": "lt", "247": "lv", "248": "ee", "289": "ge", // 289: Abkhazia (ITU lists under Georgia)
	// Europe
	"202": "gr", "204": "nl", "206": "be", "208": "fr", "212": "mc", "213": "ad",
	"214": "es", "216": "hu", "218": "ba", "219": "hr", "220": "rs", "221": "xk",
	"222": "it", "225": "va", "226": "ro", "228": "ch", "230": "cz", "231": "sk",
	"232": "at", "234": "gb", "235": "gb", "238": "dk", "240": "se", "242": "no",
	"244": "fi", "260": "pl", "262": "de", "266": "gi", "268": "pt", "270": "lu",
	"272": "ie", "274": "is", "276": "al", "278": "mt", "280": "cy", "284": "bg",
	"286": "tr", "288": "fo", "290": "gl", "292": "sm", "293": "si", "294": "mk",
	"295": "li", "297": "me",
	// Middle East / Asia
	"404": "in", "405": "in", "410": "pk", "412": "af", "413": "lk", "414": "mm",
	"415": "lb", "416": "jo", "417": "sy", "418": "iq", "419": "kw", "420": "sa",
	"421": "ye", "422": "om", "424": "ae", "425": "il", "426": "bh", "427": "qa",
	"428": "mn", "429": "np", "430": "ae", "431": "ae", "432": "ir", "440": "jp",
	"441": "jp", "450": "kr", "452": "vn", "454": "hk", "455": "mo", "456": "kh",
	"457": "la", "460": "cn", "461": "cn", "466": "tw", "467": "kp", "470": "bd",
	"472": "mv", "502": "my", "505": "au", "510": "id", "515": "ph", "520": "th",
	"525": "sg", "528": "bn", "530": "nz",
	// Americas
	"302": "ca", "310": "us", "311": "us", "312": "us", "313": "us", "314": "us",
	"315": "us", "316": "us", "334": "mx", "338": "jm", "368": "cu", "370": "do",
	"704": "gt", "706": "sv", "708": "hn", "710": "ni", "712": "cr", "714": "pa",
	"716": "pe", "722": "ar", "724": "br", "730": "cl", "732": "co", "734": "ve",
	"736": "bo", "740": "ec", "744": "py", "748": "uy",
	// Africa
	"602": "eg", "603": "dz", "604": "ma", "605": "tn", "606": "ly", "620": "gh",
	"621": "ng", "636": "et", "639": "ke", "640": "tz", "641": "ug", "655": "za",
	// satellite / international
	"901": "",
}

// CountryByMCC: "250" -> "ru"; "" when unknown.
func CountryByMCC(mcc string) string { return mccCountry[mcc] }

// Countries whose operators use 3-digit MNCs (for splitting an IMSI).
var mnc3 = map[string]bool{
	"302": true, "310": true, "311": true, "312": true, "313": true, "314": true,
	"315": true, "316": true, "334": true, "338": true, "342": true, "344": true,
	"346": true, "348": true, "354": true, "356": true, "358": true, "360": true,
	"365": true, "376": true, "708": true, "722": true, "732": true,
}

// mccmncFromIMSI: the network part of an IMSI ("250011234567890" -> "25001").
func mccmncFromIMSI(imsi string) string {
	if len(imsi) < 6 || !isDigits(imsi) {
		return ""
	}
	if mnc3[imsi[:3]] {
		return imsi[:6]
	}
	return imsi[:5]
}
