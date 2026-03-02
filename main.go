package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
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
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return "N/A"
	}
	duration := end.Sub(start).Round(time.Minute)
	totalMinutes := int(duration.Minutes())
	days := totalMinutes / (24 * 60)
	hours := (totalMinutes % (24 * 60)) / 60
	minutes := totalMinutes % 60
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if duration > 0 && len(parts) == 0 {
		return "1m"
	}
	if len(parts) == 0 {
		return "0m"
	}
	return strings.Join(parts, " ")
}

// formatDateAndTime formats Amadeus's ISO 8601 date string.
func formatDateAndTime(isoTime string) (string, time.Time) {
	layout := "2006-01-02T15:04:05"
	t, err := time.Parse(layout, isoTime)
	if err != nil {
		t, err = time.Parse(time.RFC3339, isoTime)
		if err != nil {
			log.Printf("[ERROR] Failed to parse time string '%s': %v", isoTime, err)
			return isoTime, time.Time{}
		}
	}
	return t.Format("15:04 (Jan 02)"), t
}

// getItinerarySummary calculates total duration, stops, and endpoint codes for the first leg of an offer.
func getItinerarySummary(offer map[string]interface{}) (string, string, string, int) {
	return getItinerarySummaryAt(offer, 0)
}

// getItinerarySummaryAt does the same for the itinerary at the given index (for round-trip offers).
// Returns N/A values if the offer shape is unexpected (avoids panic on malformed Amadeus responses).
func getItinerarySummaryAt(offer map[string]interface{}, itineraryIndex int) (string, string, string, int) {
	segmentsInterface, ok := offer["itineraries"].([]interface{})
	if !ok || itineraryIndex >= len(segmentsInterface) {
		return "N/A", "N/A", "N/A", 0
	}
	itinerary, ok := segmentsInterface[itineraryIndex].(map[string]interface{})
	if !ok {
		return "N/A", "N/A", "N/A", 0
	}
	segments, ok := itinerary["segments"].([]interface{})
	if !ok || len(segments) == 0 {
		return "N/A", "N/A", "N/A", 0
	}
	firstSegment, ok := segments[0].(map[string]interface{})
	if !ok {
		return "N/A", "N/A", "N/A", 0
	}
	lastSegment, ok := segments[len(segments)-1].(map[string]interface{})
	if !ok {
		return "N/A", "N/A", "N/A", 0
	}
	dep, _ := firstSegment["departure"].(map[string]interface{})
	arr, _ := lastSegment["arrival"].(map[string]interface{})
	if dep == nil || arr == nil {
		return "N/A", "N/A", "N/A", 0
	}
	startCode, _ := dep["iataCode"].(string)
	endCode, _ := arr["iataCode"].(string)
	atDep, _ := dep["at"].(string)
	atArr, _ := arr["at"].(string)
	_, startTime := formatDateAndTime(atDep)
	_, endTime := formatDateAndTime(atArr)
	totalDuration := formatDuration(startTime, endTime)
	stops := len(segments) - 1
	return startCode, endCode, totalDuration, stops
}

// buildDetailedItinerary formats one itinerary leg for the detail view. itineraryIndex is 0 for first leg, 1 for return.
func buildDetailedItinerary(offer map[string]interface{}, dictionaries map[string]interface{}, direction string, itineraryIndex int) string {
	var msg strings.Builder
	segmentsInterface, ok := offer["itineraries"].([]interface{})
	if !ok || itineraryIndex >= len(segmentsInterface) {
		return fmt.Sprintf("Itinerary details for %s unavailable.", direction)
	}
	itinerary := segmentsInterface[itineraryIndex].(map[string]interface{})
	segments, ok := itinerary["segments"].([]interface{})
	if !ok || len(segments) == 0 {
		return fmt.Sprintf("Itinerary details for %s unavailable.", direction)
	}

	price := extractRawPrice(offer)
	if price > 0 {
		msg.WriteString(fmt.Sprintf("✈️ **%s FLIGHT DETAILS** | **$%.2f USD** ✈️\n", strings.ToUpper(direction), price))
	} else {
		msg.WriteString(fmt.Sprintf("✈️ **%s FLIGHT DETAILS** ✈️\n", strings.ToUpper(direction)))
	}
	msg.WriteString("— — — — — — — — — — — — — —\n")

	var prevArrivalTime time.Time
	var prevArrivalCode string

	for i, segInterface := range segments {
		segment := segInterface.(map[string]interface{})
		departure := segment["departure"].(map[string]interface{})
		arrival := segment["arrival"].(map[string]interface{})
		depTimeStr, depTime := formatDateAndTime(departure["at"].(string))
		arrTimeStr, arrTime := formatDateAndTime(arrival["at"].(string))
		depCode := departure["iataCode"].(string)
		arrCode := arrival["iataCode"].(string)

		if i > 0 {
			layoverDuration := formatDuration(prevArrivalTime, depTime)
			if layoverDuration != "0m" && layoverDuration != "N/A" {
				msg.WriteString(fmt.Sprintf("\n   🕦 Layover in **%s**: %s\n\n", prevArrivalCode, layoverDuration))
			}
		}

		carrierCode := segment["carrierCode"].(string)
		flightNumber := segment["number"].(string)
		airlineName := carrierCode
		if carriers, ok := dictionaries["carriers"].(map[string]interface{}); ok {
			if name, found := carriers[carrierCode].(string); found {
				airlineName = name
			}
		}

		flightTime := formatDuration(depTime, arrTime)
		msg.WriteString(fmt.Sprintf("   🛫 **%s** (%s) → **%s** (%s)\n", depCode, depTimeStr, arrCode, arrTimeStr))
		msg.WriteString(fmt.Sprintf("     _Airline:_ %s (%s%s)\n     _Duration:_ %s\n", airlineName, carrierCode, flightNumber, flightTime))

		prevArrivalTime = arrTime
		prevArrivalCode = arrCode
	}
	return msg.String()
}

// buildDealMessage formats a single FullRoundTrip deal into the summary view.
func buildDealMessage(deal FullRoundTrip, index, offset int, key string) (string, tgbotapi.InlineKeyboardMarkup) {
	separator := "— — — — — — — — — — — — — —"
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("✨ **DEAL #%d** | 💲 **$%.2f USD** ✨\n", index, deal.TotalCost))
	msg.WriteString(separator + "\n")

	outStart, outEnd, outDur, outStops := getItinerarySummary(deal.OutboundFlight)
	stopStrOut := "Direct"
	if outStops > 0 {
		stopStrOut = fmt.Sprintf("%d stop(s)", outStops)
	}
	msg.WriteString(fmt.Sprintf("🛫 **Outbound:** %s → %s | 🕒 %s (%s)\n", outStart, outEnd, outDur, stopStrOut))

	retStart, retEnd, retDur, retStops := getItinerarySummary(deal.ReturnFlight)
	stopStrRet := "Direct"
	if retStops > 0 {
		stopStrRet = fmt.Sprintf("%d stop(s)", retStops)
	}
	msg.WriteString(fmt.Sprintf("🛬 **Return:** %s → %s | 🕒 %s (%s)\n", retStart, retEnd, retDur, stopStrRet))

	outboundID := deal.OutboundFlight["id"].(string)
	returnID := deal.ReturnFlight["id"].(string)
	msg.WriteString(fmt.Sprintf("🆔 Outbound `%s` | Return `%s`", outboundID, returnID))

	// The offer index for month deals is (pageStart + i) which is `index - 1`
	outboundCallback := fmt.Sprintf("DETAILS_%s_%d_0_%d", key, index-1, offset)
	returnCallback := fmt.Sprintf("DETAILS_%s_%d_1_%d", key, index-1, offset)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Show Outbound Details", outboundCallback),
			tgbotapi.NewInlineKeyboardButtonData("Show Return Details", returnCallback),
		),
	)
	return msg.String(), keyboard
}

// NEW HELPER to display a page of results, used by pagination and back buttons.
func displayFlightPage(bot *tgbotapi.BotAPI, chatID int64, messageID int, key string, offset int) {
	var msgText strings.Builder
	var rows [][]tgbotapi.InlineKeyboardButton
	separator := "— — — — — — — — — — — — — —"

	if strings.HasPrefix(key, "DEAL_") {
		maxOffers := 2
		storedMapSlice, ok := flightStore[chatID][key]
		if !ok {
			// Handle expired session
			return
		}

		dealsJSON := storedMapSlice[0]["deals_json"].(string)
		var allDeals []FullRoundTrip
		json.Unmarshal([]byte(dealsJSON), &allDeals)

		totalOffers := len(allDeals)
		pageStart := offset
		pageEnd := offset + maxOffers
		if pageEnd > totalOffers {
			pageEnd = totalOffers
		}

		keyParts := strings.Split(key, "_")
		dealParams := strings.Split(keyParts[1], "-")
		origin, destination, monthStr, duration := dealParams[0], dealParams[1], dealParams[2], dealParams[3]

		msgText.WriteString(fmt.Sprintf("💰 **Month Deals %s ➡️ %s**\n", origin, destination))
		msgText.WriteString(fmt.Sprintf("   _For %s, %s days duration_\n", monthStr, duration))
		msgText.WriteString(fmt.Sprintf("   Showing results %d-%d of **%d**:\n\n", pageStart+1, pageEnd, totalOffers))

		for i := pageStart; i < pageEnd; i++ {
			dealMsg, dealKeyboard := buildDealMessage(allDeals[i], i+1, offset, key)
			msgText.WriteString(dealMsg + "\n")
			msgText.WriteString(separator + "\n\n")
			rows = append(rows, dealKeyboard.InlineKeyboard...)
		}

		var navRow []tgbotapi.InlineKeyboardButton
		if offset > 0 {
			prevOffset := offset - maxOffers
			if prevOffset < 0 {
				prevOffset = 0
			}
			navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("PREV_%s_%d", key, prevOffset)))
		}
		if pageEnd < totalOffers {
			nextOffset := offset + maxOffers
			navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("NEXT_%s_%d", key, nextOffset)))
		}
		if len(navRow) > 0 {
			rows = append(rows, navRow)
		}
	} else if strings.HasPrefix(key, "FLIGHTS_") {
		storedMapSlice, ok := flightStore[chatID][key]
		if !ok {
			bot.Send(tgbotapi.NewEditMessageText(chatID, messageID, "Search session expired. Run /flights again."))
			return
		}
		offersRaw := storedMapSlice[0]["offers"]
		offers, ok := offersRaw.([]map[string]interface{})
		if !ok {
			if slice, ok := offersRaw.([]interface{}); ok {
				offers = make([]map[string]interface{}, 0, len(slice))
				for _, v := range slice {
					if m, ok := v.(map[string]interface{}); ok {
						offers = append(offers, m)
					}
				}
			}
		}
		if len(offers) == 0 {
			bot.Send(tgbotapi.NewEditMessageText(chatID, messageID, "No flight data available. Try the search again."))
			return
		}
		const maxPerPage = 5
		totalOffers := len(offers)
		pageStart := offset
		pageEnd := offset + maxPerPage
		if pageEnd > totalOffers {
			pageEnd = totalOffers
		}
		keyParts := strings.Split(key, "_")
		if len(keyParts) < 4 {
			bot.Send(tgbotapi.NewEditMessageText(chatID, messageID, "Invalid session. Run /flights again."))
			return
		}
		origin, dest := keyParts[1], keyParts[2]
		depDate := keyParts[3]
		retDate := ""
		if len(keyParts) >= 5 {
			retDate = keyParts[4]
		}
		msgText.WriteString(fmt.Sprintf("✈️ **Flights %s → %s**\n", origin, dest))
		msgText.WriteString(fmt.Sprintf("   _%s", depDate))
		if retDate != "" {
			msgText.WriteString(fmt.Sprintf(" — %s_", retDate))
		}
		msgText.WriteString(fmt.Sprintf("\n   Showing %d-%d of **%d**:\n\n", pageStart+1, pageEnd, totalOffers))
		for i := pageStart; i < pageEnd; i++ {
			offer := offers[i]
			price := extractRawPrice(offer)
			msgText.WriteString(fmt.Sprintf("✨ **Offer #%d** | 💲 **$%.2f USD** ✨\n", i+1, price))
			msgText.WriteString(separator + "\n")
			outStart, outEnd, outDur, outStops := getItinerarySummaryAt(offer, 0)
			stopStr := "Direct"
			if outStops > 0 {
				stopStr = fmt.Sprintf("%d stop(s)", outStops)
			}
			msgText.WriteString(fmt.Sprintf("🛫 **Outbound:** %s → %s | 🕒 %s (%s)\n", outStart, outEnd, outDur, stopStr))
			itineraries, _ := offer["itineraries"].([]interface{})
			if len(itineraries) > 1 {
				retStart, retEnd, retDur, retStops := getItinerarySummaryAt(offer, 1)
				stopStrRet := "Direct"
				if retStops > 0 {
					stopStrRet = fmt.Sprintf("%d stop(s)", retStops)
				}
				msgText.WriteString(fmt.Sprintf("🛬 **Return:** %s → %s | 🕒 %s (%s)\n", retStart, retEnd, retDur, stopStrRet))
			}
			msgText.WriteString("\n")
			outCallback := fmt.Sprintf("DETAILS_%s_%d_0_%d", key, i, offset)
			retCallback := fmt.Sprintf("DETAILS_%s_%d_1_%d", key, i, offset)
			if len(itineraries) > 1 {
				rows = append(rows, tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("Outbound Details", outCallback),
					tgbotapi.NewInlineKeyboardButtonData("Return Details", retCallback),
				))
			} else {
				rows = append(rows, tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("Details", outCallback),
				))
			}
			msgText.WriteString(separator + "\n\n")
		}
		var navRow []tgbotapi.InlineKeyboardButton
		if offset > 0 {
			prevOffset := offset - maxPerPage
			if prevOffset < 0 {
				prevOffset = 0
			}
			navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("PREV_%s_%d", key, prevOffset)))
		}
		if pageEnd < totalOffers {
			nextOffset := offset + maxPerPage
			navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("NEXT_%s_%d", key, nextOffset)))
		}
		if len(navRow) > 0 {
			rows = append(rows, navRow)
		}
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText.String())
	editMsg.ParseMode = "Markdown"
	if len(rows) > 0 {
		keyboard := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
		editMsg.ReplyMarkup = &keyboard
	}
	bot.Send(editMsg)
}

func handleCallback(bot *tgbotapi.BotAPI, update tgbotapi.Update) {
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID
	data := update.CallbackQuery.Data
	parts := strings.Split(data, "_")
	action := parts[0]

	// Answer the callback query to remove the "loading" state from the button
	defer bot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, ""))

	switch action {
	case "DETAILS":
		var key string
		var offerIndex, directionIndex, originalOffset int

		// DETAILS_DEAL_KEY-PART_0_0_0 or DETAILS_FLIGHTS_ORIGIN_DEST_DEP_RET_offerIdx_dirIdx_offset
		if parts[1] == "DEAL" && len(parts) >= 6 {
			key = fmt.Sprintf("%s_%s", parts[1], parts[2])
			offerIndex, _ = strconv.Atoi(parts[3])
			directionIndex, _ = strconv.Atoi(parts[4])
			originalOffset, _ = strconv.Atoi(parts[5])
		} else if parts[1] == "FLIGHTS" && len(parts) >= 8 {
			key = strings.Join(parts[1:len(parts)-3], "_")
			offerIndex, _ = strconv.Atoi(parts[len(parts)-3])
			directionIndex, _ = strconv.Atoi(parts[len(parts)-2])
			originalOffset, _ = strconv.Atoi(parts[len(parts)-1])
		} else {
			key = parts[1]
			offerIndex, _ = strconv.Atoi(parts[2])
			directionIndex, _ = strconv.Atoi(parts[3])
			originalOffset, _ = strconv.Atoi(parts[4])
		}

		storedMapSlice, ok := flightStore[chatID][key]
		if !ok {
			bot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "Search session expired."))
			return
		}

		var offer map[string]interface{}
		var dictionaries map[string]interface{}
		directionStr := "Outbound"
		itineraryIdx := 0

		if strings.HasPrefix(key, "DEAL_") {
			var allDeals []FullRoundTrip
			dealsJSON := storedMapSlice[0]["deals_json"].(string)
			json.Unmarshal([]byte(dealsJSON), &allDeals)

			if offerIndex >= len(allDeals) {
				return
			}
			deal := allDeals[offerIndex]
			dictionaries = deal.Dictionaries
			if directionIndex == 0 {
				offer = deal.OutboundFlight
			} else {
				offer = deal.ReturnFlight
				directionStr = "Return"
			}
			itineraryIdx = 0 // each leg is a single-itinerary offer
		} else if strings.HasPrefix(key, "FLIGHTS_") {
			offersRaw := storedMapSlice[0]["offers"]
			dictRaw := storedMapSlice[0]["dictionaries"]
			offers, ok := offersRaw.([]map[string]interface{})
			if !ok {
				bot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "Search session expired."))
				return
			}
			if offerIndex >= len(offers) {
				return
			}
			offer = offers[offerIndex]
			if dictRaw != nil {
				dictionaries, _ = dictRaw.(map[string]interface{})
			}
			if directionIndex == 1 {
				directionStr = "Return"
			}
			itineraryIdx = directionIndex
		} else {
			bot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "Search session expired."))
			return
		}

		detailText := buildDetailedItinerary(offer, dictionaries, directionStr, itineraryIdx)
		backCallbackData := fmt.Sprintf("BACK_%s_%d", key, originalOffset)
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("⬅️ Back to Results", backCallbackData),
			),
		)

		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, detailText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &keyboard
		bot.Send(editMsg)

	case "BACK", "PREV", "NEXT":
		var key string
		var offset int

		if parts[1] == "DEAL" && len(parts) >= 4 {
			key = fmt.Sprintf("%s_%s", parts[1], parts[2])
			offset, _ = strconv.Atoi(parts[3])
		} else if parts[1] == "FLIGHTS" && len(parts) >= 5 {
			key = strings.Join(parts[1:len(parts)-1], "_")
			offset, _ = strconv.Atoi(parts[len(parts)-1])
		} else {
			key = parts[1]
			offset, _ = strconv.Atoi(parts[2])
		}

		displayFlightPage(bot, chatID, messageID, key, offset)
	}
}

// handleCommands processes incoming commands.
func handleCommands(bot *tgbotapi.BotAPI, update tgbotapi.Update) {
	if update.Message == nil {
		return
	}

	log.Printf("[MSG] Received command from user %d: %s", update.Message.From.ID, update.Message.Text)

	cmd := update.Message.Command()
	if at := strings.Index(cmd, "@"); at != -1 {
		cmd = cmd[:at] // "flights@botname" -> "flights" in groups
	}
	switch cmd {
	case "start":
		startText := `Welcome to FlightCaptain! ✈️
Now using the **Amadeus Live API**.

Use /flights FROM TO DATE [RETURN_DATE] to search for flights.
Use /month_deals FROM TO YYYY-MM DURATION_DAYS to find the cheapest round trips.`
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, startText)
		msg.ParseMode = "Markdown"
		bot.Send(msg)

	case "help":
		helpText := `Commands (Using Amadeus Live API):

/flights FROM TO DATE [RETURN_DATE] - Search flights.
/month_deals FROM TO YYYY-MM DURATION_DAYS - Finds cheap round trips for a fixed duration.
Example: /month_deals TLV BER 2025-12 7
/help - Show this message.`
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, helpText)
		bot.Send(msg)

	case "flights":
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[PANIC] /flights: %v", r)
					bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("❌ Something went wrong: %v. Try different dates or check airport codes (e.g. NAP = Naples).", r)))
				}
			}()
			parts := strings.Split(update.Message.Text, " ")
			if len(parts) < 4 {
				bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "Usage: /flights FROM TO DATE [RETURN_DATE]\nExample: /flights TLV BER 2026-04-10 2026-04-17"))
				return
			}
			origin := strings.ToUpper(parts[1])
			dest := strings.ToUpper(parts[2])
			depDate := parts[3]
			returnDate := ""
			if len(parts) >= 5 {
				returnDate = parts[4]
			}
			if _, err := time.Parse("2006-01-02", depDate); err != nil {
				bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "Invalid departure date. Use YYYY-MM-DD."))
				return
			}
			if returnDate != "" {
				if _, err := time.Parse("2006-01-02", returnDate); err != nil {
					bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "Invalid return date. Use YYYY-MM-DD."))
					return
				}
			}
			searchingMsg := fmt.Sprintf("✈️ Searching flights: %s → %s (%s", origin, dest, depDate)
			if returnDate != "" {
				searchingMsg += " — " + returnDate
			}
			searchingMsg += ")..."
			initialMsg := tgbotapi.NewMessage(update.Message.Chat.ID, searchingMsg)
			sentMsg, _ := bot.Send(initialMsg)
			resp, err := amadeusClient.FlightOffersSearch(origin, dest, depDate, returnDate, 0, 15)
			if err != nil {
				bot.Send(tgbotapi.NewEditMessageText(sentMsg.Chat.ID, sentMsg.MessageID, fmt.Sprintf("❌ Search error: %v\n\n_Tip: Try different dates or check IATA codes (e.g. NAP = Naples, Italy)._", err)))
				return
			}
			if len(resp.Data) == 0 {
				bot.Send(tgbotapi.NewEditMessageText(sentMsg.Chat.ID, sentMsg.MessageID, "😔 No flights found for these dates.\n\n_Try different dates or check airport codes (e.g. NAP = Naples, Italy)._"))
				return
			}
			flightsKey := fmt.Sprintf("FLIGHTS_%s_%s_%s", origin, dest, depDate)
			if returnDate != "" {
				flightsKey += "_" + returnDate
			}
			if flightStore[sentMsg.Chat.ID] == nil {
				flightStore[sentMsg.Chat.ID] = make(map[string][]map[string]interface{})
			}
			flightStore[sentMsg.Chat.ID][flightsKey] = []map[string]interface{}{
				{"offers": resp.Data, "dictionaries": resp.Dictionaries},
			}
			displayFlightPage(bot, sentMsg.Chat.ID, sentMsg.MessageID, flightsKey, 0)
		}()

	case "month_deals":
		parts := strings.Split(update.Message.Text, " ")
		if len(parts) != 5 {
			bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "Usage: /month_deals FROM TO YYYY-MM DURATION_DAYS\nExample: /month_deals TLV BER 2025-12 7"))
			return
		}

		origin, destination, monthStr, durationStr := strings.ToUpper(parts[1]), strings.ToUpper(parts[2]), parts[3], parts[4]

		monthTime, err := time.Parse("2006-01", monthStr)
		if err != nil {
			bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "Invalid month format. Use YYYY-MM."))
			return
		}
		duration, err := strconv.Atoi(durationStr)
		if err != nil || duration <= 0 {
			bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "Invalid duration. Must be a positive integer."))
			return
		}

		initialMsg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("✈️ Searching month deals: %s ➡️ %s for %s (%d days). This may take a minute...", origin, destination, monthStr, duration))
		sentMsg, _ := bot.Send(initialMsg)

		deals, err := amadeusClient.SearchMonthDeals(origin, destination, monthTime, duration)
		if err != nil {
			bot.Send(tgbotapi.NewEditMessageText(sentMsg.Chat.ID, sentMsg.MessageID, fmt.Sprintf("❌ Search error: %v", err)))
			return
		}
		if len(deals) == 0 {
			bot.Send(tgbotapi.NewEditMessageText(sentMsg.Chat.ID, sentMsg.MessageID, "😔 No round-trip deals found."))
			return
		}

		dealKey := fmt.Sprintf("DEAL_%s-%s-%s-%s", origin, destination, monthStr, durationStr)
		allDealsJSON, _ := json.Marshal(deals)
		if flightStore[sentMsg.Chat.ID] == nil {
			flightStore[sentMsg.Chat.ID] = make(map[string][]map[string]interface{})
		}
		flightStore[sentMsg.Chat.ID][dealKey] = []map[string]interface{}{{"deals_json": string(allDealsJSON)}}

		// Use the new helper to display the first page
		displayFlightPage(bot, sentMsg.Chat.ID, sentMsg.MessageID, dealKey, 0)

	default:
		bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "Unknown command. Use /help."))
	}
}

// healthHandler responds to Render health checks and root requests.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("FlightCaptain OK"))
}

// Main entry point
func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Note: .env file not found.")
	}

	// Bind port first so Render detects it before any bot init (which may exit)
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	addr := "0.0.0.0:" + port
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", addr, err)
	}
	log.Printf("Listening on %s for health checks", addr)
	mux := http.NewServeMux()
	mux.HandleFunc("/", healthHandler)
	mux.HandleFunc("/health", healthHandler)
	go func() {
		if err := http.Serve(listener, mux); err != nil {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		log.Println("TELEGRAM_BOT_TOKEN is not set. HTTP server is up; set the token in Render Environment to enable the bot.")
		select {} // block forever so Render sees the open port and deploy succeeds
	}
	amadeusClient = NewAmadeusClient()
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("Telegram bot init failed: %v", err)
	}
	log.Printf("✅ Authorized on account %s. Bot is running...", bot.Self.UserName)
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)
	for update := range updates {
		if update.CallbackQuery != nil {
			handleCallback(bot, update)
		} else if update.Message != nil && update.Message.Text != "" &&
			(update.Message.IsCommand() || update.Message.Text[0] == '/') {
			handleCommands(bot, update)
		}
	}
}
