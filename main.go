package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// Game представляет игру в крестики-нолики
type Game struct {
	ID      string    `json:"id"`
	Board   [9]string `json:"board"`
	Players []Player  `json:"players"`
	Turn    int       `json:"turn"`   // 0 или 1 - чей ход
	Status  string    `json:"status"` // "waiting", "playing", "finished"
	Winner  string    `json:"winner"` // "", "X", "O", "draw"
	Created time.Time `json:"created"`
}

// Player представляет игрока
type Player struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Symbol string          `json:"symbol"` // "X" или "O"
	Conn   *websocket.Conn `json:"-"`
}

// GameManager управляет всеми играми
type GameManager struct {
	games map[string]*Game
	mutex sync.RWMutex
}

// Message для WebSocket коммуникации
type Message struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// MoveData для передачи хода
type MoveData struct {
	GameID   string `json:"gameId"`
	Position int    `json:"position"`
	PlayerID string `json:"playerId"`
}

var (
	gameManager = &GameManager{
		games: make(map[string]*Game),
	}
	// upgrader = websocket.Upgrader{
	// 	CheckOrigin: func(r *http.Request) bool {
	// 		// В продакшене проверяем только доверенные домены
	// 		origin := r.Header.Get("Origin")
	// 		return origin == "https://telegram.org" ||
	// 			origin == "https://web.telegram.org" ||
	// 			// Добавьте ваши домены
	// 			origin == "https://yourusername.github.io" ||
	// 			// Для разработки
	// 			origin == "http://localhost:3000" ||
	// 			origin == "http://localhost:8080"
	// 	},
	// }

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			// Разрешаем все origins для разработки
			// В продакшене лучше указать конкретные домены
			return true
		},
	}
)

// generateGameID создает уникальный ID для игры
func generateGameID() string {
	chars := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, 6)
	for i := range result {
		result[i] = chars[rand.Intn(len(chars))]
	}
	return string(result)
}

// Очистка старых игр (запускается в горутине)
func cleanupOldGames() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		gameManager.mutex.Lock()
		now := time.Now()
		for id, game := range gameManager.games {
			// Удаляем игры старше 2 часов
			if now.Sub(game.Created) > 2*time.Hour {
				delete(gameManager.games, id)
				log.Printf("Удалена старая игра: %s", id)
			}
		}
		gameManager.mutex.Unlock()
	}
}

// createGame создает новую игру
func (gm *GameManager) createGame(playerID, playerName string) *Game {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	gameID := generateGameID()
	// Проверяем уникальность ID
	for gm.games[gameID] != nil {
		gameID = generateGameID()
	}

	game := &Game{
		ID:      gameID,
		Board:   [9]string{},
		Players: []Player{{ID: playerID, Name: playerName, Symbol: "X"}},
		Turn:    0,
		Status:  "waiting",
		Created: time.Now(),
	}

	gm.games[gameID] = game
	log.Printf("Создана игра %s игроком %s", gameID, playerName)
	return game
}

// joinGame присоединяет игрока к игре
func (gm *GameManager) joinGame(gameID, playerID, playerName string) (*Game, error) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	game, exists := gm.games[gameID]
	if !exists {
		return nil, fmt.Errorf("игра не найдена")
	}

	if len(game.Players) >= 2 {
		return nil, fmt.Errorf("игра уже полная")
	}

	if game.Status != "waiting" {
		return nil, fmt.Errorf("игра уже началась")
	}

	// Проверяем, не присоединяется ли тот же игрок
	for _, p := range game.Players {
		if p.ID == playerID {
			return game, nil
		}
	}

	game.Players = append(game.Players, Player{
		ID:     playerID,
		Name:   playerName,
		Symbol: "O",
	})

	if len(game.Players) == 2 {
		game.Status = "playing"
		log.Printf("Игра %s началась: %s vs %s", gameID, game.Players[0].Name, game.Players[1].Name)
	}

	return game, nil
}

// makeMove делает ход в игре
func (gm *GameManager) makeMove(gameID, playerID string, position int) (*Game, error) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	game, exists := gm.games[gameID]
	if !exists {
		return nil, fmt.Errorf("игра не найдена")
	}

	if game.Status != "playing" {
		return nil, fmt.Errorf("игра не активна")
	}

	if position < 0 || position > 8 {
		return nil, fmt.Errorf("неверная позиция")
	}

	if game.Board[position] != "" {
		return nil, fmt.Errorf("позиция уже занята")
	}

	// Проверяем, чей ход
	currentPlayer := game.Players[game.Turn]
	if currentPlayer.ID != playerID {
		return nil, fmt.Errorf("не ваш ход")
	}

	// Делаем ход
	game.Board[position] = currentPlayer.Symbol

	// Проверяем победу
	if winner := checkWinner(game.Board); winner != "" {
		game.Status = "finished"
		game.Winner = winner
		log.Printf("Игра %s завершена, победитель: %s", gameID, winner)
	} else if isBoardFull(game.Board) {
		game.Status = "finished"
		game.Winner = "draw"
		log.Printf("Игра %s завершена ничьей", gameID)
	} else {
		// Переключаем ход
		game.Turn = 1 - game.Turn
	}

	return game, nil
}

// checkWinner проверяет победителя
func checkWinner(board [9]string) string {
	wins := [][]int{
		{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, // строки
		{0, 3, 6}, {1, 4, 7}, {2, 5, 8}, // столбцы
		{0, 4, 8}, {2, 4, 6}, // диагонали
	}

	for _, win := range wins {
		if board[win[0]] != "" &&
			board[win[0]] == board[win[1]] &&
			board[win[1]] == board[win[2]] {
			return board[win[0]]
		}
	}
	return ""
}

// isBoardFull проверяет, заполнена ли доска
func isBoardFull(board [9]string) bool {
	for _, cell := range board {
		if cell == "" {
			return false
		}
	}
	return true
}

// broadcastToGame отправляет сообщение всем игрокам в игре
func (gm *GameManager) broadcastToGame(gameID string, message Message) {
	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	game, exists := gm.games[gameID]
	if !exists {
		return
	}

	for i, player := range game.Players {
		if player.Conn != nil {
			if err := player.Conn.WriteJSON(message); err != nil {
				log.Printf("Ошибка отправки сообщения игроку %s: %v", player.ID, err)
				// Очищаем соединение при ошибке
				game.Players[i].Conn = nil
			}
		}
	}
}

// HTTP обработчики

// healthHandler для проверки состояния сервиса
func healthHandler(w http.ResponseWriter, r *http.Request) {
	gameManager.mutex.RLock()
	gameCount := len(gameManager.games)
	gameManager.mutex.RUnlock()

	response := map[string]interface{}{
		"status": "ok",
		"games":  gameCount,
		"time":   time.Now().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// createGameHandler создает новую игру
func createGameHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlayerID   string `json:"playerId"`
		PlayerName string `json:"playerName"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Неверный JSON", http.StatusBadRequest)
		return
	}

	if req.PlayerID == "" || req.PlayerName == "" {
		http.Error(w, "Не указан ID или имя игрока", http.StatusBadRequest)
		return
	}

	game := gameManager.createGame(req.PlayerID, req.PlayerName)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(game)
}

// joinGameHandler присоединяет к игре
func joinGameHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GameID     string `json:"gameId"`
		PlayerID   string `json:"playerId"`
		PlayerName string `json:"playerName"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Неверный JSON", http.StatusBadRequest)
		return
	}

	if req.GameID == "" || req.PlayerID == "" || req.PlayerName == "" {
		http.Error(w, "Не указаны обязательные поля", http.StatusBadRequest)
		return
	}

	game, err := gameManager.joinGame(req.GameID, req.PlayerID, req.PlayerName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Уведомляем всех игроков об обновлении
	gameManager.broadcastToGame(req.GameID, Message{
		Type: "gameUpdate",
		Data: game,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(game)
}

// getGameHandler получает информацию об игре
func getGameHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	gameID := vars["gameId"]

	if gameID == "" {
		http.Error(w, "Не указан ID игры", http.StatusBadRequest)
		return
	}

	gameManager.mutex.RLock()
	game, exists := gameManager.games[gameID]
	gameManager.mutex.RUnlock()

	if !exists {
		http.Error(w, "Игра не найдена", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(game)
}

// websocketHandler обрабатывает WebSocket соединения
func websocketHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("WebSocket запрос от: %s, Origin: %s", r.RemoteAddr, r.Header.Get("Origin"))

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Ошибка WebSocket upgrade: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("WebSocket соединение установлено: %s", r.RemoteAddr)

	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket неожиданное закрытие: %v", err)
			}
			break
		}

		switch msg.Type {
		case "join":
			data, _ := json.Marshal(msg.Data)
			var joinData struct {
				GameID   string `json:"gameId"`
				PlayerID string `json:"playerId"`
			}
			if err := json.Unmarshal(data, &joinData); err != nil {
				log.Printf("Ошибка парсинга join данных: %v", err)
				continue
			}

			// Сохраняем соединение для игрока
			gameManager.mutex.Lock()
			if game, exists := gameManager.games[joinData.GameID]; exists {
				for i, player := range game.Players {
					if player.ID == joinData.PlayerID {
						game.Players[i].Conn = conn
						log.Printf("Игрок %s подключился к игре %s", player.Name, joinData.GameID)
						break
					}
				}
			}
			gameManager.mutex.Unlock()

		case "move":
			data, _ := json.Marshal(msg.Data)
			var moveData MoveData
			if err := json.Unmarshal(data, &moveData); err != nil {
				log.Printf("Ошибка парсинга move данных: %v", err)
				continue
			}

			game, err := gameManager.makeMove(moveData.GameID, moveData.PlayerID, moveData.Position)
			if err != nil {
				conn.WriteJSON(Message{
					Type: "error",
					Data: map[string]string{"message": err.Error()},
				})
				continue
			}

			// Отправляем обновление всем игрокам
			gameManager.broadcastToGame(moveData.GameID, Message{
				Type: "gameUpdate",
				Data: game,
			})
		}
	}
}

// corsMiddleware добавляет CORS заголовки
// func corsMiddleware(next http.Handler) http.Handler {
// 	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
// 		origin := r.Header.Get("Origin")

// 		// Список разрешенных доменов
// 		allowedOrigins := []string{
// 			"https://telegram.org",
// 			"https://web.telegram.org",
// 			"https://gametictie.onrender.com/", // Замените на ваш GitHub Pages URL
// 			"http://localhost:3000",
// 			"http://localhost:8080",
// 		}

// 		// Проверяем origin
// 		for _, allowed := range allowedOrigins {
// 			if origin == allowed {
// 				w.Header().Set("Access-Control-Allow-Origin", origin)
// 				break
// 			}
// 		}

// 		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
// 		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
// 		w.Header().Set("Access-Control-Allow-Credentials", "true")

// 		if r.Method == "OPTIONS" {
// 			w.WriteHeader(http.StatusOK)
// 			return
// 		}

// 		next.ServeHTTP(w, r)
// 	})
// }

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// getPort возвращает порт из переменной окружения или 8080 по умолчанию
func getPort() string {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return port
}

func main() {
	rand.Seed(time.Now().UnixNano())

	// Запускаем очистку старых игр в фоне
	go cleanupOldGames()

	r := mux.NewRouter()

	// Health check endpoint
	r.HandleFunc("/health", healthHandler).Methods("GET")

	// API routes
	api := r.PathPrefix("/api").Subrouter()
	api.HandleFunc("/games", createGameHandler).Methods("POST")
	api.HandleFunc("/games/join", joinGameHandler).Methods("POST")
	api.HandleFunc("/games/{gameId}", getGameHandler).Methods("GET")
	api.HandleFunc("/ws", websocketHandler)

	// Статические файлы (если нужны)
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./static/")))

	// Применяем CORS middleware
	handler := corsMiddleware(r)

	port := getPort()
	fmt.Printf("🚀 Сервер запущен на порту %s\n", port)
	fmt.Println("📱 Готов для Telegram Mini App!")

	log.Fatal(http.ListenAndServe(":"+port, handler))
}
