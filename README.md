# ✈️ Flight Captain — Telegram Flight Search Bot

**Flight Captain** is a Go-based Telegram bot that helps users search for real-time flight deals using the **Amadeus Self-Service Flight Offers Search API**.  
It provides both **one-way** and **round-trip** flight searches, supports **pagination** via inline buttons, and shows live flight data with pricing, airlines, and schedules.

---

## 🚀 Features

- 🔍 **Search flights** using `/flights FROM TO DATE`  
  Example:  
/flights TLV HND 2026-04-15

markdown
Copy code

- 🔁 **Round-trip support** with duration parameter  
Example:  
/flights TLV HND 2026-04-15 7d

markdown
Copy code
→ Searches for return flights 7 days after the outbound flight.

- 💰 **Live pricing** powered by [Amadeus API](https://developers.amadeus.com/)

- 🧭 **Multiple results & pagination**  
Displays 5 flight options by default and a “Load more” button for additional results.

- 🧳 **Flight details view**  
Tap any flight to expand airline info, layovers, and full timing details.

- ⚡ **Optimized performance**  
Efficient pagination and lightweight API queries to minimize response time.

- 🔒 **Session-based state management**  
Inline button presses (like “Load more” or “Flight 2”) are tied to each user’s session.

---

## 🧩 Tech Stack

- **Language:** Go (Golang)
- **Telegram API:** [go-telegram-bot-api](https://github.com/go-telegram-bot-api/telegram-bot-api)
- **Flight Data:** [Amadeus Self-Service APIs](https://developers.amadeus.com/)
- **Environment Loader:** [godotenv](https://github.com/joho/godotenv)
- **Hosting:** Works locally or on cloud (Render, Railway, Fly.io, etc.)

---

## ⚙️ Installation

```bash
1. Clone the repository

git clone https://github.com/YOUR_USERNAME/flight-captain.git
cd flight-captain
```
```bash
2. Set up environment variables
Create a .env file in the project root:
```
```bash
Copy code
TELEGRAM_BOT_TOKEN=your_telegram_bot_token
AMADEUS_CLIENT_ID=your_amadeus_client_id
AMADEUS_CLIENT_SECRET=your_amadeus_client_secret
```
```bash
3. Install dependencies
bash
Copy code
go mod tidy
```
```bash
4. Run the bot
bash
Copy code
go run main.go
```
## 🧠 Command Reference
---
### Command	Description
- ** /start	Welcome message and usage guide **
- ** /help	Shows example commands **
- ** /flights FROM TO DATE	One-way flight search **
- ** /flights FROM TO DATE DURATION	Round-trip flight search (e.g. 7d, 10d) **

#### 📡 API Integration
The bot uses the following Amadeus endpoints:

### Authentication:
POST https://test.api.amadeus.com/v1/security/oauth2/token

### Flight Offers Search:
GET https://test.api.amadeus.com/v2/shopping/flight-offers

### Amadeus documentation:
https://developers.amadeus.com/self-service/category/air/api-doc/flight-offers-search

#### Made with ❤️ and ☕ by a Developer who hates slow layovers.
