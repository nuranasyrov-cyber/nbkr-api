package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/charmap"
)

type NBKRRates struct {
	XMLName    xml.Name       `xml:"CurrencyRates"`
	Date       string         `xml:"Date,attr"`
	Currencies []NBKRCurrency `xml:"Currency"`
}

type NBKRCurrency struct {
	ISOCode string `xml:"ISOCode,attr"`
	Nominal int    `xml:"Nominal"`
	Value   string `xml:"Value"`
}

type CurrencyResponse struct {
	ISOCode string `json:"iso_code"`
	Nominal int    `json:"nominal"`
	Value   string `json:"value"`
}

// Кэш
var (
	cacheMu      sync.Mutex
	cachedRates  *NBKRRates
	cacheExpires time.Time
)

func main() {
	http.HandleFunc("/", homeHandler)
	http.HandleFunc("/api/rates", ratesHandler)
	http.HandleFunc("/api/currencies", currenciesHandler)
	http.HandleFunc("/api/convert", convertHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Println("Server running on port " + port)
	http.ListenAndServe(":"+port, nil)
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Welcome to the API!")
}

func fetchNBKR(date string) ([]byte, error) {
	url := "https://www.nbkr.kg/XML/daily.xml"
	if date != "" {
		url = "https://www.nbkr.kg/XML/daily.xml?date=" + date
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	decoded, err := charmap.Windows1251.NewDecoder().Bytes(raw)
	if err != nil {
		return nil, err
	}

	fixed := bytes.Replace(decoded, []byte(`encoding="windows-1251"`), []byte(`encoding="utf-8"`), 1)
	return fixed, nil
}

func getRates(date string) (*NBKRRates, error) {
	// Кэш работает только для текущего дня (без даты)
	if date == "" {
		cacheMu.Lock()
		defer cacheMu.Unlock()

		if cachedRates != nil && time.Now().Before(cacheExpires) {
			return cachedRates, nil
		}
	}

	body, err := fetchNBKR(date)
	if err != nil {
		return nil, err
	}

	var rates NBKRRates
	if err := xml.Unmarshal(body, &rates); err != nil {
		return nil, err
	}

	if date == "" {
		cachedRates = &rates
		cacheExpires = time.Now().Add(1 * time.Hour)
	}

	return &rates, nil
}

func parseValue(v string) (float64, error) {
	normalized := strings.Replace(v, ",", ".", 1)
	return strconv.ParseFloat(normalized, 64)
}

// GET /api/rates?currency=USD&date=01.01.2026
func currenciesHandler(w http.ResponseWriter, r *http.Request) {
	rates, err := getRates("")
	if err != nil {
		http.Error(w, "Failed to fetch currencies: "+err.Error(), http.StatusBadGateway)
		return
	}

	currencies := make([]string, 0, len(rates.Currencies)+1)
	currencies = append(currencies, "KGS")
	for _, c := range rates.Currencies {
		currencies = append(currencies, c.ISOCode)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"date":       rates.Date,
		"currencies": currencies,
	})
}

func ratesHandler(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	currency := strings.ToUpper(r.URL.Query().Get("currency"))

	rates, err := getRates(date)
	if err != nil {
		http.Error(w, "Failed to fetch rates: "+err.Error(), http.StatusBadGateway)
		return
	}

	result := make([]CurrencyResponse, 0, len(rates.Currencies))
	for _, c := range rates.Currencies {
		if currency != "" && c.ISOCode != currency {
			continue
		}
		result = append(result, CurrencyResponse{
			ISOCode: c.ISOCode,
			Nominal: c.Nominal,
			Value:   c.Value,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"date":  rates.Date,
		"rates": result,
	})
}

// GET /api/convert?from=USD&to=KGS&amount=100
func convertHandler(w http.ResponseWriter, r *http.Request) {
	from := strings.ToUpper(r.URL.Query().Get("from"))
	to := strings.ToUpper(r.URL.Query().Get("to"))
	amountStr := r.URL.Query().Get("amount")

	if from == "" || to == "" || amountStr == "" {
		http.Error(w, "Params required: from, to, amount", http.StatusBadRequest)
		return
	}

	amount, err := strconv.ParseFloat(amountStr, 64)
	if err != nil {
		http.Error(w, "Invalid amount", http.StatusBadRequest)
		return
	}

	rates, err := getRates("")
	if err != nil {
		http.Error(w, "Failed to fetch rates: "+err.Error(), http.StatusBadGateway)
		return
	}

	rateMap := make(map[string]float64)
	nominalMap := make(map[string]int)
	for _, c := range rates.Currencies {
		val, err := parseValue(c.Value)
		if err == nil {
			rateMap[c.ISOCode] = val
			nominalMap[c.ISOCode] = c.Nominal
		}
	}
	// KGS — базовая валюта (1 KGS = 1 KGS)
	rateMap["KGS"] = 1
	nominalMap["KGS"] = 1

	fromRate, okFrom := rateMap[from]
	fromNominal, _ := nominalMap[from]
	toRate, okTo := rateMap[to]
	toNominal, _ := nominalMap[to]

	if !okFrom {
		http.Error(w, "Unknown currency: "+from, http.StatusBadRequest)
		return
	}
	if !okTo {
		http.Error(w, "Unknown currency: "+to, http.StatusBadRequest)
		return
	}

	// amount * (fromRate / fromNominal) / (toRate / toNominal)
	amountInKGS := amount * (fromRate / float64(fromNominal))
	result := amountInKGS / (toRate / float64(toNominal))

	fromRatePerUnit := fromRate / float64(fromNominal)
	toRatePerUnit := toRate / float64(toNominal)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"from":         from,
		"to":           to,
		"amount":       amount,
		"result":       fmt.Sprintf("%.4f", result),
		"date":         rates.Date,
		"rate_from":    fmt.Sprintf("1 %s = %.4f KGS", from, fromRatePerUnit),
		"rate_to":      fmt.Sprintf("1 %s = %.4f KGS", to, toRatePerUnit),
	})
}
