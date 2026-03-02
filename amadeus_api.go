package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NOTE: Changed base URL from test to live API
const amadeusBaseURL = "https://api.amadeus.com" // Now using the live API environment
const maxConcurrentTrips = 5                     // Max number of simultaneous date-pair searches to avoid 429 errors

// --- API Response Structures ---

// APIResponse encapsulates the full structured response from the Flight Offers API.
type APIResponse struct {
	Data         []map[string]interface{} `json:"data"`
	Dictionaries map[string]interface{}   `json:"dictionaries"`
	Meta         map[string]interface{}   `json:"meta"`
}

// FullRoundTrip encapsulates a complete round trip flight deal for /month_deals.
type FullRoundTrip struct {
	OutboundFlight map[string]interface{}
	ReturnFlight   map[string]interface{}
	TotalCost      float64
	OutboundDate   string
	ReturnDate     string
	// Store dictionaries (carriers, airports, etc.) for detailed message building in main.go
	Dictionaries map[string]interface{}
}

// --- Utility Helpers ---

// extractRawPrice safely extracts the price from a raw Amadeus flight offer map by checking
// the nested "grandTotal" or "total" fields and converting it to float64.
// This is critical for getting the correct price from the raw offer JSON structure.
func extractRawPrice(flight map[string]interface{}) float64 {
	// 1. Check if price is already a direct float64 (if it was simplified before storage)
	if price, ok := flight["price"].(float64); ok {
		return price
	}

	// 2. Check for the price map structure
	rawPrice, ok := flight["price"]
	if !ok {
		return 0
	}

	priceMap, ok := rawPrice.(map[string]interface{})
	if !ok {
		return 0
	}

	// 3. Extract the price string from "grandTotal" or "total"
	var priceStr string
	if totalStr, found := priceMap["grandTotal"].(string); found {
		priceStr = totalStr
	} else if totalStr, found := priceMap["total"].(string); found {
		// Fallback to "total" if "grandTotal" is missing (Amadeus response structure varies)
		priceStr = totalStr
	} else {
		return 0
	}

	p, err := strconv.ParseFloat(priceStr, 64)
	if err != nil {
		log.Printf("[PRICE_ERROR] Failed to parse float '%s': %v", priceStr, err)
		return 0
	}
	return p
}

// mergeDictionaries combines two dictionary maps from Amadeus responses.
func mergeDictionaries(d1, d2 map[string]interface{}) map[string]interface{} {
	if d1 == nil {
		return d2
	}
	if d2 == nil {
		return d1
	}

	merged := make(map[string]interface{})
	// Start with d1
	for k, v := range d1 {
		merged[k] = v
	}

	// Merge d2 into merged (overwriting d1's values for inner map keys)
	for k, v := range d2 {
		if existing, ok := merged[k].(map[string]interface{}); ok {
			if incoming, ok := v.(map[string]interface{}); ok {
				// Safely merge inner maps (e.g., 'carriers', 'locations')
				for innerK, innerV := range incoming {
					existing[innerK] = innerV
				}
				merged[k] = existing
			} else {
				merged[k] = v
			}
		} else {
			merged[k] = v
		}
	}
	return merged
}

// --- Amadeus Client ---

// AmadeusClient handles API key, token management, and requests.
type AmadeusClient struct {
	clientID     string
	clientSecret string
	token        string
	tokenExpiry  time.Time
	mu           sync.Mutex // Mutex to protect token updates
}

// NewAmadeusClient initializes the client and loads credentials from the environment.
func NewAmadeusClient() *AmadeusClient {
	clientID := os.Getenv("AMADEUS_CLIENT_ID")
	clientSecret := os.Getenv("AMADEUS_CLIENT_SECRET")

	if clientID == "" || clientSecret == "" {
		// NOTE: This now points to the live API, so credentials are required.
		log.Fatal("AMADEUS_CLIENT_ID and AMADEUS_CLIENT_SECRET must be set in the .env file.")
	}

	client := &AmadeusClient{
		clientID:     clientID,
		clientSecret: clientSecret,
	}

	if err := client.getAccessToken(); err != nil {
		log.Fatalf("Failed to get initial Amadeus token: %v", err)
	}
	return client
}

// getAccessToken refreshes the Amadeus access token.
func (c *AmadeusClient) getAccessToken() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if the current token is still valid (with a 5-minute buffer)
	if time.Now().Add(5*time.Minute).Before(c.tokenExpiry) && c.token != "" {
		return nil
	}

	log.Println("[AMADEUS_AUTH] Attempting to refresh access token...")

	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", c.clientID)
	data.Set("client_secret", c.clientSecret)

	req, err := http.NewRequest("POST", amadeusBaseURL+"/v1/security/oauth2/token", strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("could not create auth request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send auth request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("auth request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"` // in seconds
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode auth response: %w", err)
	}

	c.token = result.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).Add(-1 * time.Minute)
	log.Printf("[AMADEUS_AUTH] Token refreshed. Expires in %d seconds.", result.ExpiresIn)

	return nil
}

// makeAPIRequest handles token refreshing, request signing, and response decoding.
func (c *AmadeusClient) makeAPIRequest(method, endpoint string, queryParams url.Values) (map[string]interface{}, error) {
	if err := c.getAccessToken(); err != nil {
		return nil, fmt.Errorf("failed to refresh access token: %w", err)
	}

	u, err := url.Parse(amadeusBaseURL + endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint URL: %w", err)
	}
	u.RawQuery = queryParams.Encode()
	fullURL := u.String()

	log.Printf("[AMADEUS_API] Making request to %s", fullURL)

	req, err := http.NewRequest(method, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create request: %w", err)
	}

	c.mu.Lock()
	req.Header.Set("Authorization", "Bearer "+c.token)
	c.mu.Unlock()

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[AMADEUS_ERROR] Status %d for %s: %s", resp.StatusCode, endpoint, string(body))

		var apiError struct {
			Errors []struct {
				Title  string `json:"title"`
				Detail string `json:"detail"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(body, &apiError); err == nil && len(apiError.Errors) > 0 {
			errMsg := fmt.Sprintf("%s: %s", apiError.Errors[0].Title, apiError.Errors[0].Detail)
			return nil, errors.New(errMsg)
		}
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode API response: %w", err)
	}

	return result, nil
}

// FlightOffersSearch performs the main flight search, including support for pagination via offset.
func (c *AmadeusClient) FlightOffersSearch(origin, destination, departureDate, returnDate string, offset int, maxOffers int) (APIResponse, error) {
	queryParams := url.Values{}
	queryParams.Set("originLocationCode", origin)
	queryParams.Set("destinationLocationCode", destination)
	queryParams.Set("departureDate", departureDate)
	queryParams.Set("adults", "1") // Fixed at 1 adult for simplicity
	queryParams.Set("max", strconv.Itoa(maxOffers))

	// Set currency to USD for consistency
	queryParams.Set("currencyCode", "USD")

	if returnDate != "" {
		// If returnDate is provided, it's a round trip search
		queryParams.Set("returnDate", returnDate)
	}

	// Only include offset if > 0 to avoid API 400 error on page 1 (where offset should be omitted)
	if offset > 0 {
		queryParams.Set("offset", strconv.Itoa(offset))
	}

	rawResult, err := c.makeAPIRequest("GET", "/v2/shopping/flight-offers", queryParams)
	if err != nil {
		return APIResponse{}, fmt.Errorf("API request failed: %w", err)
	}

	// Process the raw map response into the structured APIResponse
	var resp APIResponse
	if data, ok := rawResult["data"].([]interface{}); ok {
		offers := make([]map[string]interface{}, 0, len(data))
		for _, item := range data {
			if offerMap, ok := item.(map[string]interface{}); ok {
				offers = append(offers, offerMap)
			}
		}
		resp.Data = offers
	}
	if meta, ok := rawResult["meta"].(map[string]interface{}); ok {
		resp.Meta = meta
	}
	if dictionaries, ok := rawResult["dictionaries"].(map[string]interface{}); ok {
		resp.Dictionaries = dictionaries
	}

	return resp, nil
}

// searchCheapestSingleLeg is a helper for SearchMonthDeal, finding the single cheapest flight.
// Now returns the cheapest offer map, the dictionary map, and an error.
func (c *AmadeusClient) searchCheapestSingleLeg(origin, destination, date string) (map[string]interface{}, map[string]interface{}, error) {
	// Use FlightOffersSearch with offset=0 (omitted) and maxOffers=1
	resp, err := c.FlightOffersSearch(origin, destination, date, "", 0, 1)
	if err != nil {
		return nil, nil, err
	}
	if len(resp.Data) == 0 {
		return nil, nil, errors.New("no offers found")
	}
	// Return the cheapest (first) offer and the dictionaries
	return resp.Data[0], resp.Dictionaries, nil
}

// SearchMonthDeals finds the cheapest round-trip flights for a fixed duration
// across all days of the specified month.
func (c *AmadeusClient) SearchMonthDeals(origin, destination string, month time.Time, durationDays int) ([]FullRoundTrip, error) {
	log.Printf("[MONTH] Starting search for %s to %s in %s for duration %d days.",
		origin, destination, month.Format("January 2006"), durationDays)

	// Determine the first day of the month to start the iteration
	currentOutboundDate := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, month.Location())

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, maxConcurrentTrips)
	tripsChan := make(chan FullRoundTrip, 31)

	// Iterate up to 31 times to cover all possible starting days of a month
	for i := 0; i < 31; i++ {

		outboundDate := currentOutboundDate.AddDate(0, 0, i)

		// Check if we have moved into the next month
		if outboundDate.Month() != month.Month() {
			break // Stop iterating when we exceed the target month
		}

		// Calculate return date
		returnDate := outboundDate.AddDate(0, 0, durationDays)

		// Convert dates to string format for API call
		outboundDateStr := outboundDate.Format("2006-01-02")
		returnDateStr := returnDate.Format("2006-01-02")

		wg.Add(1)

		go func(outboundStr, returnStr string) {
			semaphore <- struct{}{} // Acquire token (limit concurrent goroutines)
			defer func() {
				<-semaphore // Release token
				wg.Done()
			}()

			// --- 1. Find cheapest outbound flight ---
			cheapestOutbound, outboundDictionaries, err := c.searchCheapestSingleLeg(origin, destination, outboundStr)
			if err != nil {
				// We don't log this as an error, as it's common for no flights to exist on a given day
				// log.Printf("[MONTH:TRIP] Outbound search failed for %s: %v", outboundStr, err)
				return
			}

			// --- 2. Find cheapest return flight ---
			cheapestReturn, returnDictionaries, err := c.searchCheapestSingleLeg(destination, origin, returnStr)
			if err != nil {
				// log.Printf("[MONTH:TRIP] Return search failed for %s: %v", returnStr, err)
				return
			}

			// --- 3. Combine and send to channel ---
			totalCost := extractRawPrice(cheapestOutbound) + extractRawPrice(cheapestReturn)

			// Merge the dictionaries from both legs to ensure full coverage of all used codes
			combinedDictionaries := mergeDictionaries(outboundDictionaries, returnDictionaries)

			trip := FullRoundTrip{
				OutboundFlight: cheapestOutbound, // Store full raw flight offer
				ReturnFlight:   cheapestReturn,   // Store full raw flight offer
				TotalCost:      totalCost,
				OutboundDate:   outboundStr,
				ReturnDate:     returnStr,
				Dictionaries:   combinedDictionaries, // Store combined dictionaries
			}

			tripsChan <- trip
			log.Printf("[MONTH:TRIP] Found round-trip deal (%s/%s): $%.2f", outboundStr, returnStr, totalCost)

		}(outboundDateStr, returnDateStr)
	}

	wg.Wait()
	close(tripsChan)

	var allTrips []FullRoundTrip
	for trip := range tripsChan {
		allTrips = append(allTrips, trip)
	}

	log.Printf("[MONTH] Finished concurrent search loop. Found %d deals.", len(allTrips))

	// Sort final results by total cost
	sort.Slice(allTrips, func(i, j int) bool {
		return allTrips[i].TotalCost < allTrips[j].TotalCost
	})

	return allTrips, nil
}
