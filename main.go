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

// Game представляет игру
type Game struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`   // "tictactoe" или "battleship"
	Board        [9]string `json:"board"`  // Для крестиков-ноликов
	Boards       []Board   `json:"boards"` // Для морского боя
	Players      []Player  `json:"players"`
	Turn         int       `json:"turn"`   // 0 или 1 - чей ход
	Status       string    `json:"status"` // "waiting", "playing", "finished", "restart_requested"
	Winner       string    `json:"winner"` // "", "X", "O", "draw", "player1", "player2"
	Created      time.Time `json:"created"`
	RestartVotes []string  `json:"restartVotes"` // ID игроков, проголосовавших за повтор
}

// Board для морского боя (10x10)
type Board struct {
	Grid  [10][10]string `json:"grid"`  // Сетка игрока
	Ships []Ship         `json:"ships"` // Корабли игрока
	Ready bool           `json:"ready"` // Готов ли игрок
}

// Ship представляет корабль в морском бое
type Ship struct {
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Length    int    `json:"length"`
	Direction string `json:"direction"` // "horizontal" или "vertical"
	Hits      int    `json:"hits"`
}

// Player представляет игрока
type Player struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Symbol string          `json:"symbol"` // "X" или "O" для крестиков-ноликов
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

// MoveData для передачи хода в крестики-нолики
type MoveData struct {
	GameID   string `json:"gameId"`
	Position int    `json:"position"`
	PlayerID string `json:"playerId"`
}

// AttackData для атаки в морском бое
type AttackData struct {
	GameID   string `json:"gameId"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	PlayerID string `json:"playerId"`
}

// ShipPlacementData для размещения кораблей
type ShipPlacementData struct {
	GameID   string `json:"gameId"`
	PlayerID string `json:"playerId"`
	Ships    []Ship `json:"ships"`
}

// RestartVoteData для голосования за повтор
type RestartVoteData struct {
	GameID   string `json:"gameId"`
	PlayerID string `json:"playerId"`
}

var (
	gameManager = &GameManager{
		games: make(map[string]*Game),
	}
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
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

// Очистка старых игр
func cleanupOldGames() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		gameManager.mutex.Lock()
		now := time.Now()
		for id, game := range gameManager.games {
			if now.Sub(game.Created) > 2*time.Hour {
				delete(gameManager.games, id)
				log.Printf("Удалена старая игра: %s", id)
			}
		}
		gameManager.mutex.Unlock()
	}
}

// createGame создает новую игру
func (gm *GameManager) createGame(playerID, playerName, gameType string) *Game {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	gameID := generateGameID()
	for gm.games[gameID] != nil {
		gameID = generateGameID()
	}

	game := &Game{
		ID:           gameID,
		Type:         gameType,
		Players:      []Player{{ID: playerID, Name: playerName, Symbol: "X"}},
		Turn:         0,
		Status:       "waiting",
		Created:      time.Now(),
		RestartVotes: []string{},
	}

	if gameType == "tictactoe" {
		game.Board = [9]string{}
	} else if gameType == "battleship" {
		game.Boards = make([]Board, 2)
		for i := range game.Boards {
			game.Boards[i] = Board{
				Grid:  [10][10]string{},
				Ships: []Ship{},
				Ready: false,
			}
		}
	}

	gm.games[gameID] = game
	log.Printf("Создана игра %s (%s) игроком %s", gameID, gameType, playerName)
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

	for _, p := range game.Players {
		if p.ID == playerID {
			return game, nil
		}
	}

	symbol := "O"
	if game.Type == "battleship" {
		symbol = ""
	}

	game.Players = append(game.Players, Player{
		ID:     playerID,
		Name:   playerName,
		Symbol: symbol,
	})

	if len(game.Players) == 2 {
		if game.Type == "tictactoe" {
			game.Status = "playing"
		} else if game.Type == "battleship" {
			game.Status = "setup" // Фаза расстановки кораблей
		}
		log.Printf("Игра %s (%s) началась: %s vs %s", gameID, game.Type, game.Players[0].Name, game.Players[1].Name)
	}

	return game, nil
}

// restartGame перезапускает игру
func (gm *GameManager) restartGame(gameID string) (*Game, error) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	game, exists := gm.games[gameID]
	if !exists {
		return nil, fmt.Errorf("игра не найдена")
	}

	// Сбрасываем состояние игры
	if game.Type == "tictactoe" {
		game.Board = [9]string{}
		game.Status = "playing"
	} else if game.Type == "battleship" {
		for i := range game.Boards {
			game.Boards[i] = Board{
				Grid:  [10][10]string{},
				Ships: []Ship{},
				Ready: false,
			}
		}
		game.Status = "setup"
	}

	game.Turn = 0
	game.Winner = ""
	game.RestartVotes = []string{}

	log.Printf("Игра %s перезапущена", gameID)
	return game, nil
}

// voteRestart голосует за перезапуск игры
func (gm *GameManager) voteRestart(gameID, playerID string) (*Game, error) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	game, exists := gm.games[gameID]
	if !exists {
		return nil, fmt.Errorf("игра не найдена")
	}

	if game.Status != "finished" {
		return nil, fmt.Errorf("игра не завершена")
	}

	// Проверяем, не голосовал ли уже этот игрок
	for _, vote := range game.RestartVotes {
		if vote == playerID {
			return game, nil // Уже голосовал
		}
	}

	game.RestartVotes = append(game.RestartVotes, playerID)

	// Если все игроки проголосовали, перезапускаем игру
	if len(game.RestartVotes) == len(game.Players) {
		return gm.restartGameInternal(game)
	}

	game.Status = "restart_requested"
	return game, nil
}

func (gm *GameManager) restartGameInternal(game *Game) (*Game, error) {
	if game.Type == "tictactoe" {
		game.Board = [9]string{}
		game.Status = "playing"
	} else if game.Type == "battleship" {
		for i := range game.Boards {
			game.Boards[i] = Board{
				Grid:  [10][10]string{},
				Ships: []Ship{},
				Ready: false,
			}
		}
		game.Status = "setup"
	}

	game.Turn = 0
	game.Winner = ""
	game.RestartVotes = []string{}

	return game, nil
}

// makeMove делает ход в крестики-нолики
func (gm *GameManager) makeMove(gameID, playerID string, position int) (*Game, error) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	game, exists := gm.games[gameID]
	if !exists {
		return nil, fmt.Errorf("игра не найдена")
	}

	if game.Type != "tictactoe" {
		return nil, fmt.Errorf("неверный тип игры")
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

	currentPlayer := game.Players[game.Turn]
	if currentPlayer.ID != playerID {
		return nil, fmt.Errorf("не ваш ход")
	}

	game.Board[position] = currentPlayer.Symbol

	if winner := checkWinnerTicTacToe(game.Board); winner != "" {
		game.Status = "finished"
		game.Winner = winner
		log.Printf("Игра %s завершена, победитель: %s", gameID, winner)
	} else if isBoardFull(game.Board) {
		game.Status = "finished"
		game.Winner = "draw"
		log.Printf("Игра %s завершена ничьей", gameID)
	} else {
		game.Turn = 1 - game.Turn
	}

	return game, nil
}

// placeShips размещает корабли для морского боя
func (gm *GameManager) placeShips(gameID, playerID string, ships []Ship) (*Game, error) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	game, exists := gm.games[gameID]
	if !exists {
		return nil, fmt.Errorf("игра не найдена")
	}

	if game.Type != "battleship" {
		return nil, fmt.Errorf("неверный тип игры")
	}

	if game.Status != "setup" {
		return nil, fmt.Errorf("фаза расстановки завершена")
	}

	// Найдем индекс игрока
	playerIndex := -1
	for i, player := range game.Players {
		if player.ID == playerID {
			playerIndex = i
			break
		}
	}

	if playerIndex == -1 {
		return nil, fmt.Errorf("игрок не найден")
	}

	// Проверяем корректность расстановки кораблей
	if !validateShipPlacement(ships) {
		return nil, fmt.Errorf("некорректная расстановка кораблей")
	}

	// Размещаем корабли
	game.Boards[playerIndex].Ships = ships
	game.Boards[playerIndex].Ready = true

	// Обновляем сетку
	for i := range game.Boards[playerIndex].Grid {
		for j := range game.Boards[playerIndex].Grid[i] {
			game.Boards[playerIndex].Grid[i][j] = ""
		}
	}

	for _, ship := range ships {
		for i := 0; i < ship.Length; i++ {
			x, y := ship.X, ship.Y
			if ship.Direction == "horizontal" {
				x += i
			} else {
				y += i
			}
			game.Boards[playerIndex].Grid[y][x] = "ship"
		}
	}

	// Если оба игрока готовы, начинаем игру
	if len(game.Players) == 2 && game.Boards[0].Ready && game.Boards[1].Ready {
		game.Status = "playing"
		log.Printf("Игра морской бой %s началась", gameID)
	}

	return game, nil
}

// attack делает атаку в морском бое
func (gm *GameManager) attack(gameID, playerID string, x, y int) (*Game, error) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	game, exists := gm.games[gameID]
	if !exists {
		return nil, fmt.Errorf("игра не найдена")
	}

	if game.Type != "battleship" {
		return nil, fmt.Errorf("неверный тип игры")
	}

	if game.Status != "playing" {
		return nil, fmt.Errorf("игра не активна")
	}

	if x < 0 || x > 9 || y < 0 || y > 9 {
		return nil, fmt.Errorf("неверные координаты")
	}

	currentPlayer := game.Players[game.Turn]
	if currentPlayer.ID != playerID {
		return nil, fmt.Errorf("не ваш ход")
	}

	// Индекс противника
	targetIndex := 1 - game.Turn
	target := &game.Boards[targetIndex]

	// Проверяем, не атаковали ли уже эту клетку
	if target.Grid[y][x] == "hit" || target.Grid[y][x] == "miss" {
		return nil, fmt.Errorf("клетка уже атакована")
	}

	hit := false
	if target.Grid[y][x] == "ship" {
		target.Grid[y][x] = "hit"
		hit = true

		// Проверяем, потоплен ли корабль
		for i, ship := range target.Ships {
			if isShipHit(&ship, x, y) {
				target.Ships[i].Hits++
				if target.Ships[i].Hits >= ship.Length {
					log.Printf("Корабль потоплен в игре %s", gameID)
				}
				break
			}
		}

		// Проверяем победу
		if allShipsSunk(target.Ships) {
			game.Status = "finished"
			if game.Turn == 0 {
				game.Winner = "player1"
			} else {
				game.Winner = "player2"
			}
			log.Printf("Игра морской бой %s завершена, победитель: %s", gameID, game.Winner)
		}
	} else {
		target.Grid[y][x] = "miss"
	}

	// Если промах, передаем ход
	if !hit && game.Status == "playing" {
		game.Turn = 1 - game.Turn
	}

	return game, nil
}

// Вспомогательные функции

func validateShipPlacement(ships []Ship) bool {
	// Проверяем количество кораблей: 1x4, 2x3, 3x2, 4x1
	shipCounts := map[int]int{4: 0, 3: 0, 2: 0, 1: 0}
	requiredCounts := map[int]int{4: 1, 3: 2, 2: 3, 1: 4}

	grid := [10][10]bool{}

	for _, ship := range ships {
		if ship.Length < 1 || ship.Length > 4 {
			return false
		}
		shipCounts[ship.Length]++

		// Проверяем границы и пересечения
		for i := 0; i < ship.Length; i++ {
			x, y := ship.X, ship.Y
			if ship.Direction == "horizontal" {
				x += i
			} else {
				y += i
			}

			if x < 0 || x > 9 || y < 0 || y > 9 {
				return false
			}

			if grid[y][x] {
				return false // Пересечение
			}

			// Проверяем соседние клетки
			for dx := -1; dx <= 1; dx++ {
				for dy := -1; dy <= 1; dy++ {
					nx, ny := x+dx, y+dy
					if nx >= 0 && nx < 10 && ny >= 0 && ny < 10 && grid[ny][nx] {
						return false // Корабли касаются
					}
				}
			}

			grid[y][x] = true
		}
	}

	// Проверяем количество кораблей каждого типа
	for length, required := range requiredCounts {
		if shipCounts[length] != required {
			return false
		}
	}

	return true
}

func isShipHit(ship *Ship, x, y int) bool {
	for i := 0; i < ship.Length; i++ {
		sx, sy := ship.X, ship.Y
		if ship.Direction == "horizontal" {
			sx += i
		} else {
			sy += i
		}
		if sx == x && sy == y {
			return true
		}
	}
	return false
}

func allShipsSunk(ships []Ship) bool {
	for _, ship := range ships {
		if ship.Hits < ship.Length {
			return false
		}
	}
	return true
}

func checkWinnerTicTacToe(board [9]string) string {
	wins := [][]int{
		{0, 1, 2}, {3, 4, 5}, {6, 7, 8},
		{0, 3, 6}, {1, 4, 7}, {2, 5, 8},
		{0, 4, 8}, {2, 4, 6},
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
				game.Players[i].Conn = nil
			}
		}
	}
}

// HTTP обработчики

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

func createGameHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlayerID   string `json:"playerId"`
		PlayerName string `json:"playerName"`
		GameType   string `json:"gameType"` // "tictactoe" или "battleship"
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Неверный JSON", http.StatusBadRequest)
		return
	}

	if req.PlayerID == "" || req.PlayerName == "" {
		http.Error(w, "Не указан ID или имя игрока", http.StatusBadRequest)
		return
	}

	if req.GameType == "" {
		req.GameType = "tictactoe"
	}

	if req.GameType != "tictactoe" && req.GameType != "battleship" {
		http.Error(w, "Неверный тип игры", http.StatusBadRequest)
		return
	}

	game := gameManager.createGame(req.PlayerID, req.PlayerName, req.GameType)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(game)
}

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

	gameManager.broadcastToGame(req.GameID, Message{
		Type: "gameUpdate",
		Data: game,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(game)
}

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

func websocketHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("WebSocket запрос от: %s", r.RemoteAddr)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Ошибка WebSocket upgrade: %v", err)
		return
	}
	defer conn.Close()

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
				continue
			}

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

			gameManager.broadcastToGame(moveData.GameID, Message{
				Type: "gameUpdate",
				Data: game,
			})

		case "attack":
			data, _ := json.Marshal(msg.Data)
			var attackData AttackData
			if err := json.Unmarshal(data, &attackData); err != nil {
				continue
			}

			game, err := gameManager.attack(attackData.GameID, attackData.PlayerID, attackData.X, attackData.Y)
			if err != nil {
				conn.WriteJSON(Message{
					Type: "error",
					Data: map[string]string{"message": err.Error()},
				})
				continue
			}

			gameManager.broadcastToGame(attackData.GameID, Message{
				Type: "gameUpdate",
				Data: game,
			})

		case "placeShips":
			data, _ := json.Marshal(msg.Data)
			var shipData ShipPlacementData
			if err := json.Unmarshal(data, &shipData); err != nil {
				continue
			}

			game, err := gameManager.placeShips(shipData.GameID, shipData.PlayerID, shipData.Ships)
			if err != nil {
				conn.WriteJSON(Message{
					Type: "error",
					Data: map[string]string{"message": err.Error()},
				})
				continue
			}

			gameManager.broadcastToGame(shipData.GameID, Message{
				Type: "gameUpdate",
				Data: game,
			})

		case "restartVote":
			data, _ := json.Marshal(msg.Data)
			var restartData RestartVoteData
			if err := json.Unmarshal(data, &restartData); err != nil {
				continue
			}

			game, err := gameManager.voteRestart(restartData.GameID, restartData.PlayerID)
			if err != nil {
				conn.WriteJSON(Message{
					Type: "error",
					Data: map[string]string{"message": err.Error()},
				})
				continue
			}

			gameManager.broadcastToGame(restartData.GameID, Message{
				Type: "gameUpdate",
				Data: game,
			})
		}
	}
}

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

func getPort() string {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return port
}

func main() {
	rand.Seed(time.Now().UnixNano())
	go cleanupOldGames()

	r := mux.NewRouter()
	r.HandleFunc("/health", healthHandler).Methods("GET")

	api := r.PathPrefix("/api").Subrouter()
	api.HandleFunc("/games", createGameHandler).Methods("POST")
	api.HandleFunc("/games/join", joinGameHandler).Methods("POST")
	api.HandleFunc("/games/{gameId}", getGameHandler).Methods("GET")
	api.HandleFunc("/ws", websocketHandler)

	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./static/")))

	handler := corsMiddleware(r)
	port := getPort()
	fmt.Printf("🚀 Сервер запущен на порту %s\n", port)
	fmt.Println("📱 Готов для Telegram Mini App!")

	log.Fatal(http.ListenAndServe(":"+port, handler))
}
