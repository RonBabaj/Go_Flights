package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	// External Dependencies
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

// NOTE: AmadeusClient, FullRoundTrip, and extractRawPrice are defined in amadeus_api.go
// The map stores JSON strings of []FullRoundTrip for month deals, or []map[string]interface{} for single flights.
var flightStore = make(map[int64]map[string][]map[string]interface{})
var amadeusClient *AmadeusClient // Declared globally for client access

// formatDuration calculates the difference between two time.Time objects and formats it as Dd Hh Mm.
func formatDuration(start, end time.Time) string {
	// Check for zero time or invalid range (end before start)
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return "N/A"
	}

	duration := end.Sub(start)

	// Round to the nearest minute for clean display
	duration = duration.Round(time.Minute)

	totalMinutes := int(duration.Minutes())

	days := totalMinutes / (24 * 60)
	hours := (totalMinutes % (24 * 60)) / 60
	minutes := totalMinutes % 60

	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}

	// Handle cases where duration is positive but rounds to zero (e.g., 30s)
	if duration > 0 && len(parts) == 0 {
		return "1m" // Display at least 1 minute if duration is positive
	}

	if len(parts) == 0 {
		return "0m"
	}

	return strings.Join(parts, " ")
}

// formatDateAndTime formats Amadeus's ISO 8601 date string (YYYY-MM-DDTHH:MM:SS)
// to a shorter HH:MM (Mon DD) format, and returns the time.Time object.
func formatDateAndTime(isoTime string) (string, time.Time) {
	// Attempt to parse the common format without Z or timezone offset.
	layout := "2006-01-02T15:04:05"
	t, err := time.Parse(layout, isoTime)

	if err != nil {
		// Fallback to time.RFC3339 for strings with Z or an offset
		t, err = time.Parse(time.RFC3339, isoTime)
		if err != nil {
			// Log error if parsing fails to help debugging, but return gracefully
			log.Printf("[ERROR] Failed to parse time string '%s' using multiple layouts: %v", isoTime, err)
			return isoTime, time.Time{}
		}
	}

	// Example: 19:10 (Dec 11)
	return t.Format("15:04 (Jan 02)"), t
}

// buildDetailedItinerary formats one itinerary leg (outbound or return) into a readable segment flow.
// The caller is responsible for providing the header (e.g., "🛫 **OUTBOUND**") and the price/separator.
func buildDetailedItinerary(offer map[string]interface{}, dictionaries map[string]interface{}, direction string) string {
	var msg strings.Builder

	// Get the itinerary data
	segmentsInterface, ok := offer["itineraries"].([]interface{})
	if !ok || len(segmentsInterface) == 0 {
		// This fallback is mostly for logging/debugging
		return fmt.Sprintf("   [Itinerary details for %s unavailable]\n", direction)
	}

	itinerary := segmentsInterface[0].(map[string]interface{})
	segments := itinerary["segments"].([]interface{})

	var prevArrivalTime time.Time
	var prevArrivalCode string

	// Iterate through each flight segment
	for i, segInterface := range segments {
		segment := segInterface.(map[string]interface{})

		// 1. Departure/Arrival Details
		departure := segment["departure"].(map[string]interface{})
		arrival := segment["arrival"].(map[string]interface{})

		// Correctly retrieve formatted time strings and time.Time objects
		depTimeStr, depTime := formatDateAndTime(departure["at"].(string))
		arrTimeStr, arrTime := formatDateAndTime(arrival["at"].(string))
		depCode := departure["iataCode"].(string)
		arrCode := arrival["iataCode"].(string)

		// 2. Layover Calculation (if not the first segment)
		if i > 0 {
			// Use time.Time objects directly for layover calculation
			layoverDuration := formatDuration(prevArrivalTime, depTime)

			// New, slicker layover format
			if layoverDuration != "0m" && layoverDuration != "N/A" {
				msg.WriteString(fmt.Sprintf("   🕦 Layover in **%s**: %s\n", prevArrivalCode, layoverDuration))
			}
		}

		// 3. Flight Details
		carrierCode := segment["carrierCode"].(string)
		flightNumber := segment["number"].(string)

		// Carrier code to name lookup
		airlineName := carrierCode
		if carriers, ok := dictionaries["carriers"].(map[string]interface{}); ok {
			if name, found := carriers[carrierCode].(string); found {
				airlineName = name
			}
		}

		// Use time.Time objects directly for flight duration calculation
		flightTime := formatDuration(depTime, arrTime)

		// New, slicker flight segment format
		emoji := "✈️"
		if i == 0 {
			emoji = "🛫" // First flight
		} else if i == len(segments)-1 {
			emoji = "🛬" // Final flight of the leg
		}

		msg.WriteString(fmt.Sprintf("   %s **%s** (%s) → **%s** (%s)\n", emoji, depCode, depTimeStr, arrCode, arrTimeStr))
		msg.WriteString(fmt.Sprintf("     _Carrier:_ %s (%s%s) | _Duration:_ %s\n", airlineName, carrierCode, flightNumber, flightTime))

		// Update for next segment's layover calculation
		prevArrivalTime = arrTime
		prevArrivalCode = arrCode
	}

	return msg.String()
}

// buildDealMessage formats a single FullRoundTrip deal into a readable string.
func buildDealMessage(deal FullRoundTrip, index int) string {
	totalCost := deal.TotalCost
	separator := "— — — — — — — — — — — — — —"

	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("✨ **DEAL #%d** | 💲 **$%.2f USD** ✨\n", index, totalCost))
	msg.WriteString(separator + "\n")

	// --- Outbound Flight Details ---
	outboundPrice := extractRawPrice(deal.OutboundFlight)
	msg.WriteString(fmt.Sprintf("🛫 **OUTBOUND** (%s) | **$%.2f USD**\n", deal.OutboundDate, outboundPrice))
	msg.WriteString(buildDetailedItinerary(deal.OutboundFlight, deal.Dictionaries, "Outbound"))
	msg.WriteString(separator + "\n")

	// --- Return Flight Details ---
	returnPrice := extractRawPrice(deal.ReturnFlight)
	msg.WriteString(fmt.Sprintf("🛬 **RETURN** (%s) | **$%.2f USD**\n", deal.ReturnDate, returnPrice))
	msg.WriteString(buildDetailedItinerary(deal.ReturnFlight, deal.Dictionaries, "Return"))

	// Show the flight IDs for reference
	outboundID := "N/A"
	if id, ok := deal.OutboundFlight["id"].(string); ok {
		outboundID = id
	}

	returnID := "N/A"
	if id, ok := deal.ReturnFlight["id"].(string); ok {
		returnID = id
	}

	// Summary
	msg.WriteString(fmt.Sprintf("\n💰 **TOTAL TRIP PRICE: $%.2f USD**\n", totalCost))
	msg.WriteString(fmt.Sprintf("🆔 Reference: Outbound `%s` | Return `%s`\n", outboundID, returnID))
	msg.WriteString(separator + "\n\n") // Extra newline separator between deals

	return msg.String()
}

// handleCallback processes button presses (Next Page, Prev Page).
func handleCallback(bot *tgbotapi.BotAPI, update tgbotapi.Update) {
	chatID := update.CallbackQuery.Message.Chat.ID
	data := update.CallbackQuery.Data // e.g., "NEXT_DEAL_TLV-BER-2025-12-7-7_2"

	parts := strings.Split(data, "_")

	var key string
	var currentOffset int

	// --- Determine Key and Offset based on parts length ---
	// Month Deal: ACTION_DEAL_PARAMS_OFFSET (4 parts total, key is DEAL_PARAMS)
	if len(parts) == 4 && parts[1] == "DEAL" {
		key = fmt.Sprintf("%s_%s", parts[1], parts[2])
		currentOffset, _ = strconv.Atoi(parts[3])
	} else if len(parts) == 3 {
		// Standard Flight: ACTION_KEY_OFFSET (3 parts)
		key = parts[1]
		currentOffset, _ = strconv.Atoi(parts[2])
	} else {
		callbackFail := tgbotapi.NewCallback(update.CallbackQuery.ID, fmt.Sprintf("Invalid button data structure. Received %d parts.", len(parts)))
		bot.Send(callbackFail)
		return
	}

	log.Printf("[CALLBACK] Processing action %s with full key: %s, offset: %d", parts[0], key, currentOffset)

	// Retrieve stored data for this query key
	storedMapSlice, ok := flightStore[chatID][key]

	// Check if the session is expired (data lost, likely due to process restart)
	if !ok || (strings.HasPrefix(key, "DEAL_") && len(storedMapSlice) == 0) {
		log.Printf("[CALLBACK:ERROR] Session expired for chatID %d and key %s. Store status: %v, Length: %d", chatID, key, ok, len(storedMapSlice))
		callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Search session expired or not found. Please run the search command again.")
		bot.Send(callback)
		return
	}

	maxOffers := 0 // Will be set based on the type of search
	separator := "— — — — — — — — — — — — — —"

	var msgText strings.Builder
	var row []tgbotapi.InlineKeyboardButton
	var hasButtons bool = false // Flag to track if any buttons were added

	if strings.HasPrefix(key, "DEAL_") {
		// --- Handle Month Deal Pagination (pre-calculated list) ---
		maxOffers = 2

		// 1. Retrieve the stored JSON string of []FullRoundTrip
		dealsJSON, found := storedMapSlice[0]["deals_json"].(string)
		if !found {
			callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Deal data corrupted: Missing JSON.")
			bot.Send(callback)
			return
		}

		// 2. Unmarshal the string back to []FullRoundTrip
		var allDeals []FullRoundTrip
		if err := json.Unmarshal([]byte(dealsJSON), &allDeals); err != nil {
			log.Printf("[CALLBACK:ERROR] Failed to unmarshal deals: %v", err)
			callback := tgbotapi.NewCallback(update.CallbackQuery.ID, fmt.Sprintf("Deal data corrupted: %v", err))
			bot.Send(callback)
			return
		}

		totalOffers := len(allDeals)

		// Slice the array based on offset
		pageStart := currentOffset
		pageEnd := currentOffset + maxOffers
		if pageEnd > totalOffers {
			pageEnd = totalOffers
		}

		// Recalculate the search parameters from the key for display
		keyParts := strings.Split(key, "_")
		dealParams := strings.Split(keyParts[1], "-")

		origin := dealParams[0]
		destination := dealParams[1]
		monthStr := dealParams[2]
		duration := dealParams[3]

		msgText.WriteString(fmt.Sprintf("💰 **Month Deals %s ➡️ %s**\n", origin, destination))
		msgText.WriteString(fmt.Sprintf("   _For %s, %s days duration_\n", monthStr, duration))
		msgText.WriteString(fmt.Sprintf("   Showing results %d-%d of **%d**:\n\n", pageStart+1, pageEnd, totalOffers))

		// 3. Build the page content
		for i := pageStart; i < pageEnd; i++ {
			msgText.WriteString(buildDealMessage(allDeals[i], i+1))
		}

		// --- Build Pagination Buttons ---

		// Prev button (show if not on the first page)
		if currentOffset > 0 {
			prevOffset := currentOffset - maxOffers
			if prevOffset < 0 {
				prevOffset = 0
			}
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("PREV_%s_%d", key, prevOffset)))
			hasButtons = true
		}

		// Next button (show if there are more results)
		if pageEnd < totalOffers {
			nextOffset := currentOffset + maxOffers
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("NEXT_%s_%d", key, nextOffset)))
			hasButtons = true
		}

	} else {
		// --- Handle Standard Flight Offer Pagination (requires new API call) ---

		maxOffers = 5

		// Recalculate the search parameters from the key
		keyParts := strings.Split(key, "-")
		if len(keyParts) < 3 {
			callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Invalid search key.")
			bot.Send(callback)
			return
		}

		origin := keyParts[0]
		destination := keyParts[1]
		departureDate := keyParts[2]

		returnDate := ""
		if len(keyParts) == 4 {
			returnDate = keyParts[3]
		}

		// Perform the next search
		resp, err := amadeusClient.FlightOffersSearch(origin, destination, departureDate, returnDate, currentOffset, maxOffers)

		if err != nil {
			log.Printf("[CALLBACK:ERROR] Standard Flight Search failed: %v", err)
			callback := tgbotapi.NewCallback(update.CallbackQuery.ID, fmt.Sprintf("Search failed: %v", err))
			bot.Send(callback)
			return
		}

		totalOffers := 0
		if meta, ok := resp.Meta["count"].(float64); ok {
			totalOffers = int(meta)
		}

		pageStart := currentOffset + 1
		pageEnd := currentOffset + len(resp.Data)

		msgText.WriteString(fmt.Sprintf("✈️ **Flights %s ➡️ %s** (%s)\n", origin, destination, departureDate))
		if returnDate != "" {
			msgText.WriteString(fmt.Sprintf("   _Return:_ %s\n", returnDate))
		}
		msgText.WriteString(fmt.Sprintf("   Showing results %d-%d of %d:\n\n", pageStart, pageEnd, totalOffers))

		// Clear and update the flightStore with the new page data
		flightStore[chatID][key] = resp.Data

		for i, offer := range resp.Data {
			legCost := extractRawPrice(offer)
			msgText.WriteString(fmt.Sprintf("✈️ **OPTION #%d** | **$%.2f USD**\n", i+pageStart, legCost))
			msgText.WriteString(separator + "\n")

			// --- Handle One-Way vs. Round Trip Formatting ---
			if returnDate != "" {
				// Round Trip: The offer contains two itineraries (outbound and return)
				itineraries, ok := offer["itineraries"].([]interface{})
				if ok && len(itineraries) >= 2 {
					// 1. Outbound Leg
					outboundOffer := map[string]interface{}{"itineraries": []interface{}{itineraries[0]}}
					msgText.WriteString(fmt.Sprintf("🛫 **OUTBOUND** (%s)\n", departureDate))
					msgText.WriteString(buildDetailedItinerary(outboundOffer, resp.Dictionaries, "Outbound"))
					msgText.WriteString(separator + "\n")

					// 2. Return Leg
					returnOffer := map[string]interface{}{"itineraries": []interface{}{itineraries[1]}}
					msgText.WriteString(fmt.Sprintf("🛬 **RETURN** (%s)\n", returnDate))
					msgText.WriteString(buildDetailedItinerary(returnOffer, resp.Dictionaries, "Return"))

				} else {
					msgText.WriteString("_Error: Could not parse round trip itinerary._\n")
				}
			} else {
				// One-Way
				msgText.WriteString(buildDetailedItinerary(offer, resp.Dictionaries, "One-Way"))
			}

			msgText.WriteString(separator + "\n\n")
		}

		// --- Build Pagination Buttons ---

		// Prev button
		if currentOffset > 0 {
			prevOffset := currentOffset - maxOffers
			if prevOffset < 0 {
				prevOffset = 0
			}
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("PREV_%s_%d", key, prevOffset)))
			hasButtons = true
		}

		// Next button
		if pageEnd < totalOffers {
			nextOffset := currentOffset + maxOffers
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("NEXT_%s_%d", key, nextOffset)))
			hasButtons = true
		}
	}

	// --- Edit the message ---
	editMsg := tgbotapi.NewEditMessageText(chatID, update.CallbackQuery.Message.MessageID, msgText.String())
	editMsg.ParseMode = "Markdown"

	// Only set ReplyMarkup if buttons were actually created
	if hasButtons {
		keyboard := tgbotapi.NewInlineKeyboardMarkup(row)
		editMsg.ReplyMarkup = &keyboard
	}

	_, err := bot.Send(editMsg)
	if err != nil {
		log.Printf("[CALLBACK:SEND_ERROR] Failed to edit message: %v", err)
		// Fallback message to user if formatting fails
		bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("❌ Failed to display page due to a formatting error: %v", err)))
	}

	// Answer the callback query to dismiss the loading indicator
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Loading next page...")
	bot.Send(callback)
}

// handleCommands processes incoming commands.
func handleCommands(bot *tgbotapi.BotAPI, update tgbotapi.Update) {
	if update.Message == nil {
		return
	}

	log.Printf("[MSG] Received command from user %d: %s", update.Message.From.ID, update.Message.Text)

	switch update.Message.Command() {
	case "start":
		// Using a raw string literal (backticks) for clean, multi-line text
		startText := `Welcome to FlightCaptain! ✈️
Now using the **Amadeus Live API**.

Use /flights FROM TO DATE [RETURN_DATE] to search for flights.
Use /month_deals FROM TO YYYY-MM DURATION_DAYS to search for cheap round trips.`
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, startText)
		msg.ParseMode = "Markdown"
		if _, err := bot.Send(msg); err != nil {
			log.Printf("[START:SEND_ERROR] Failed to send start message: %v", err)
		}

	case "help":
		// Removed ParseMode entirely for the help message to ensure it is sent as plain text
		// and avoids any potential Markdown parsing errors.
		helpText := `Commands (Using Amadeus Live API):

/flights FROM TO DATE [RETURN_DATE] - Search flights.
/month_deals FROM TO YYYY-MM DURATION_DAYS - Finds cheap round trips for a fixed duration.
Example: /month_deals TLV BER 2025-12 7
/help - Show this message.`
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, helpText)
		// NOTE: msg.ParseMode = "Markdown" is intentionally omitted here.
		if _, err := bot.Send(msg); err != nil {
			log.Printf("[HELP:SEND_ERROR] Failed to send help message: %v", err)
		}

	case "flights":
		parts := strings.Split(update.Message.Text, " ")
		if len(parts) < 4 {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Usage: /flights FROM TO DATE [RETURN_DATE]\nExample: /flights TLV BER 2025-10-25 2025-10-30")
			bot.Send(msg)
			return
		}

		origin := strings.ToUpper(parts[1])
		destination := strings.ToUpper(parts[2])
		departureDate := parts[3]
		returnDate := ""
		if len(parts) > 4 {
			returnDate = parts[4]
		}

		if _, err := time.Parse("2006-01-02", departureDate); err != nil {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Invalid departure date format. Use YYYY-MM-DD.")
			bot.Send(msg)
			return
		}
		if returnDate != "" {
			if _, err := time.Parse("2006-01-02", returnDate); err != nil {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Invalid return date format. Use YYYY-MM-DD.")
				bot.Send(msg)
				return
			}
		}

		offset := 0
		maxOffers := 5
		separator := "— — — — — — — — — — — — — —"

		initialMsg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("✈️ Searching for flights: %s ➡️ %s, %s...", origin, destination, departureDate))
		sentMsg, _ := bot.Send(initialMsg)

		resp, err := amadeusClient.FlightOffersSearch(origin, destination, departureDate, returnDate, offset, maxOffers)

		if err != nil {
			log.Printf("[FLIGHTS:ERROR] Search failed: %v", err)
			errMsg := fmt.Sprintf("❌ An error occurred during search: %v", err)
			bot.Send(tgbotapi.NewEditMessageText(update.Message.Chat.ID, sentMsg.MessageID, errMsg))
			return
		}

		totalOffers := 0
		if meta, ok := resp.Meta["count"].(float64); ok {
			totalOffers = int(meta)
		}

		if totalOffers == 0 {
			bot.Send(tgbotapi.NewEditMessageText(update.Message.Chat.ID, sentMsg.MessageID, fmt.Sprintf("😔 No flights found for %s ➡️ %s on %s.", origin, destination, departureDate)))
			return
		}

		var msgText strings.Builder
		pageStart := offset + 1
		pageEnd := offset + len(resp.Data)

		msgText.WriteString(fmt.Sprintf("✅ **Flights %s ➡️ %s** (%s)\n", origin, destination, departureDate))
		if returnDate != "" {
			msgText.WriteString(fmt.Sprintf("   _Return:_ %s\n", returnDate))
		}

		msgText.WriteString(fmt.Sprintf("   Showing results %d-%d of %d:\n\n", pageStart, pageEnd, totalOffers))

		key := fmt.Sprintf("%s-%s-%s", origin, destination, departureDate)
		if returnDate != "" {
			key = fmt.Sprintf("%s-%s", key, returnDate)
		}

		if flightStore[update.Message.Chat.ID] == nil {
			flightStore[update.Message.Chat.ID] = make(map[string][]map[string]interface{})
		}
		flightStore[update.Message.Chat.ID][key] = resp.Data

		for i, offer := range resp.Data {
			legCost := extractRawPrice(offer)
			msgText.WriteString(fmt.Sprintf("✈️ **OPTION #%d** | **$%.2f USD**\n", i+1, legCost))
			msgText.WriteString(separator + "\n")

			// --- Handle One-Way vs. Round Trip Formatting ---
			if returnDate != "" {
				// Round Trip: The offer contains two itineraries (outbound and return)
				itineraries, ok := offer["itineraries"].([]interface{})
				if ok && len(itineraries) >= 2 {
					// 1. Outbound Leg
					outboundOffer := map[string]interface{}{"itineraries": []interface{}{itineraries[0]}}
					msgText.WriteString(fmt.Sprintf("🛫 **OUTBOUND** (%s)\n", departureDate))
					msgText.WriteString(buildDetailedItinerary(outboundOffer, resp.Dictionaries, "Outbound"))
					msgText.WriteString(separator + "\n")

					// 2. Return Leg
					returnOffer := map[string]interface{}{"itineraries": []interface{}{itineraries[1]}}
					msgText.WriteString(fmt.Sprintf("🛬 **RETURN** (%s)\n", returnDate))
					msgText.WriteString(buildDetailedItinerary(returnOffer, resp.Dictionaries, "Return"))

				} else {
					msgText.WriteString("_Error: Could not parse round trip itinerary._\n")
				}
			} else {
				// One-Way
				msgText.WriteString(buildDetailedItinerary(offer, resp.Dictionaries, "One-Way"))
			}

			msgText.WriteString(separator + "\n\n")
		}

		var row []tgbotapi.InlineKeyboardButton
		var keyboard tgbotapi.InlineKeyboardMarkup
		var hasButtons bool = false

		if pageEnd < totalOffers {
			nextOffset := offset + maxOffers
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("NEXT_%s_%d", key, nextOffset)))
			hasButtons = true
		}

		editMsg := tgbotapi.NewEditMessageText(update.Message.Chat.ID, sentMsg.MessageID, msgText.String())
		editMsg.ParseMode = "Markdown"

		if hasButtons {
			keyboard = tgbotapi.NewInlineKeyboardMarkup(row)
			editMsg.ReplyMarkup = &keyboard
		}

		_, err = bot.Send(editMsg)
		if err != nil {
			log.Printf("[FLIGHTS:SEND_ERROR] Failed to send/edit message: %v", err)
			bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "❌ Failed to display flight results due to a formatting error. Check the logs for details."))
		}

	case "month_deals":
		log.Printf("[MSG] Received command from user %d: %s", update.Message.From.ID, update.Message.Text)
		parts := strings.Split(update.Message.Text, " ")
		if len(parts) != 5 {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Usage: /month_deals FROM TO YYYY-MM DURATION_DAYS\nExample: /month_deals TLV BER 2025-12 7")
			bot.Send(msg)
			return
		}

		origin := strings.ToUpper(parts[1])
		destination := strings.ToUpper(parts[2])
		monthStr := parts[3]
		durationStr := parts[4]

		monthTime, err := time.Parse("2006-01", monthStr)
		if err != nil {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Invalid month format. Use YYYY-MM (e.g., 2025-12).")
			bot.Send(msg)
			return
		}

		duration, err := strconv.Atoi(durationStr)
		if err != nil || duration <= 0 {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Invalid duration. Must be a positive integer in days.")
			bot.Send(msg)
			return
		}

		// Notify user of search start
		initialMsg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("✈️ Searching for month deals: %s ➡️ %s, %s, duration %d days. This may take up to a minute...", origin, destination, monthStr, duration))
		sentMsg, _ := bot.Send(initialMsg)

		// This returns the full slice of all deals (FullRoundTrip structs)
		deals, err := amadeusClient.SearchMonthDeals(origin, destination, monthTime, duration)
		if err != nil {
			log.Printf("[MONTH:ERROR] Search failed: %v", err)
			errMsg := fmt.Sprintf("❌ An error occurred during search: %v", err)
			bot.Send(tgbotapi.NewEditMessageText(update.Message.Chat.ID, sentMsg.MessageID, errMsg))
			return
		}

		totalDeals := len(deals)
		if totalDeals == 0 {
			msg := tgbotapi.NewEditMessageText(update.Message.Chat.ID, sentMsg.MessageID, fmt.Sprintf("😔 No round-trip deals found for %s to %s in %s.", origin, destination, monthStr))
			bot.Send(msg)
			return
		}

		// --- Store all deals for pagination ---
		dealKey := fmt.Sprintf("DEAL_%s-%s-%s-%s", origin, destination, monthStr, durationStr)

		allDealsJSON, _ := json.Marshal(deals)

		if flightStore[update.Message.Chat.ID] == nil {
			flightStore[update.Message.Chat.ID] = make(map[string][]map[string]interface{})
		}
		flightStore[update.Message.Chat.ID][dealKey] = []map[string]interface{}{
			{"deals_json": string(allDealsJSON)},
		}

		// --- Build Message and Keyboard for initial results (Page 1) ---

		offset := 0
		maxOffers := 2

		var msgText strings.Builder

		pageStart := offset
		pageEnd := offset + maxOffers
		if pageEnd > totalDeals {
			pageEnd = totalDeals
		}

		msgText.WriteString(fmt.Sprintf("💰 **Month Deals %s ➡️ %s**\n", origin, destination))
		msgText.WriteString(fmt.Sprintf("   _For %s, %s days duration_\n", monthStr, durationStr))
		msgText.WriteString(fmt.Sprintf("   Showing results %d-%d of **%d**:\n\n", pageStart+1, pageEnd, totalDeals))

		for i := pageStart; i < pageEnd; i++ {
			msgText.WriteString(buildDealMessage(deals[i], i+1))
		}

		// --- Build Pagination Buttons ---
		var row []tgbotapi.InlineKeyboardButton
		var keyboard tgbotapi.InlineKeyboardMarkup
		var hasButtons bool = false

		if pageEnd < totalDeals {
			nextOffset := offset + maxOffers
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("NEXT_%s_%d", dealKey, nextOffset)))
			hasButtons = true
		}

		editMsg := tgbotapi.NewEditMessageText(update.Message.Chat.ID, sentMsg.MessageID, msgText.String())
		editMsg.ParseMode = "Markdown"

		if hasButtons {
			keyboard = tgbotapi.NewInlineKeyboardMarkup(row)
			editMsg.ReplyMarkup = &keyboard
		}

		_, err = bot.Send(editMsg)
		if err != nil {
			log.Printf("[MONTH:SEND_ERROR] Failed to send/edit message: %v", err)
			bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "❌ Search completed but failed to display results due to a formatting error. The message might be too long."))
		}

	default:
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Unknown command. Use /help to see available commands.")
		bot.Send(msg)
	}
}

// Main entry point
func main() {
	// Load environment variables from .env file
	err := godotenv.Load()
	if err != nil {
		log.Println("Note: Could not find .env file. Falling back to environment variables.")
	}

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set.")
	}

	// Initialize the Amadeus client globally
	amadeusClient = NewAmadeusClient()

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}

	log.Printf("✅ Authorized on account %s. Bot is running...", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			handleCallback(bot, update)
			continue
		}

		if update.Message == nil {
			continue
		}

		// Handle commands (starts with /)
		if update.Message.IsCommand() {
			handleCommands(bot, update)
			continue
		}
	}
}
