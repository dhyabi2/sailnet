package client

// continentOf maps a country code to a coarse region, used only to prefer
// short inter-relay hops when drawing a path. Unknown codes get their own
// region so they neither attract nor repel.
func continentOf(cc string) string {
	if r, ok := regions[cc]; ok {
		return r
	}
	return cc
}

var regions = map[string]string{}

func init() {
	groups := map[string]string{
		"EU":  "AT BE BG CH CZ DE DK EE ES FI FR GB GR HR HU IE IS IT LT LU LV MD MT NL NO PL PT RO RS SE SI SK UA AL BA MK ME XK LI AD MC SM",
		"NA":  "US CA MX",
		"SA":  "AR BR CL CO EC PE UY VE BO PY",
		"EA":  "JP KR TW HK CN MO MN",
		"SE":  "SG MY TH VN ID PH KH LA MM BN",
		"SA2": "IN PK BD LK NP",
		"OC":  "AU NZ",
		"ME":  "AE SA IL TR QA KW BH OM JO IQ IR EG",
		"AF":  "ZA NG KE GH MA TN DZ ET TZ",
		"RU":  "RU BY KZ",
	}
	for region, list := range groups {
		start := 0
		for i := 0; i <= len(list); i++ {
			if i == len(list) || list[i] == ' ' {
				if i > start {
					regions[list[start:i]] = region
				}
				start = i + 1
			}
		}
	}
}
