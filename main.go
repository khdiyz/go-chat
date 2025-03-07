// main.go
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Message represents a chat message
type Message struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Content   string    `json:"content"`
	FileURL   string    `json:"fileUrl,omitempty"`
	FileName  string    `json:"fileName,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Global variables
var (
	clients   = make(map[*websocket.Conn]string) // connected clients (websocket -> username)
	broadcast = make(chan Message)               // broadcast channel
	upgrader  = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all connections
		},
	}
	minioClient *minio.Client
	bucketName  = "chat-files"
)

func main() {
	// Load environment variables
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: .env file not found, using environment variables")
	}

	// Initialize MinIO client
	initMinIO()

	// Initialize the Gin router
	router := gin.Default()

	// Serve static files
	router.Static("/static", "./static")
	router.StaticFile("/", "./static/index.html")

	// API routes
	router.GET("/ws", handleConnections)
	router.POST("/upload", handleFileUpload)
	router.GET("/download/:filename", handleFileDownload)

	// Start listening for incoming messages
	go handleMessages()

	// Start the server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Server starting on port %s...", port)
	if err := router.Run(":" + port); err != nil {
		log.Fatal("Error starting server: ", err)
	}
}

// Initialize MinIO client
func initMinIO() {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	accessKey := os.Getenv("MINIO_ACCESS_KEY")
	secretKey := os.Getenv("MINIO_SECRET_KEY")
	useSSL := os.Getenv("MINIO_USE_SSL") == "true"

	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	if secretKey == "" {
		secretKey = "minioadmin"
	}

	// Initialize MinIO client
	var err error
	minioClient, err = minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		log.Fatalf("Error initializing MinIO client: %v", err)
	}

	// Create bucket if it doesn't exist
	ctx := context.Background()
	exists, err := minioClient.BucketExists(ctx, bucketName)
	if err != nil {
		log.Fatalf("Error checking if bucket exists: %v", err)
	}
	if !exists {
		err = minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
		if err != nil {
			log.Fatalf("Error creating bucket: %v", err)
		}
		log.Printf("Created bucket: %s", bucketName)

		// Set bucket policy to allow public read access
		policy := `{
			"Version": "2012-10-17",
			"Statement": [
				{
					"Effect": "Allow",
					"Principal": {"AWS": ["*"]},
					"Action": ["s3:GetObject"],
					"Resource": ["arn:aws:s3:::` + bucketName + `/*"]
				}
			]
		}`
		err = minioClient.SetBucketPolicy(ctx, bucketName, policy)
		if err != nil {
			log.Fatalf("Error setting bucket policy: %v", err)
		}
	}
}

// Handle WebSocket connections
func handleConnections(c *gin.Context) {
	// Upgrade GET request to WebSocket
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Error upgrading to WebSocket: %v", err)
		return
	}
	defer ws.Close()

	// Read username from the URL query parameter
	username := c.Query("username")
	if username == "" {
		username = "anonymous-" + uuid.New().String()[0:8]
	}

	// Register new client
	clients[ws] = username
	log.Printf("New client connected: %s", username)

	// Send welcome message
	welcomeMsg := Message{
		ID:        uuid.New().String(),
		Username:  "System",
		Content:   fmt.Sprintf("Welcome, %s! You are now connected.", username),
		Timestamp: time.Now(),
	}
	err = ws.WriteJSON(welcomeMsg)
	if err != nil {
		log.Printf("Error sending welcome message: %v", err)
		delete(clients, ws)
		return
	}

	// Notify all clients about new user
	broadcast <- Message{
		ID:        uuid.New().String(),
		Username:  "System",
		Content:   fmt.Sprintf("%s has joined the chat", username),
		Timestamp: time.Now(),
	}

	// Listen for messages from this client
	for {
		var msg Message
		err := ws.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading message: %v", err)
			delete(clients, ws)
			// Notify all clients about disconnected user
			broadcast <- Message{
				ID:        uuid.New().String(),
				Username:  "System",
				Content:   fmt.Sprintf("%s has left the chat", username),
				Timestamp: time.Now(),
			}
			break
		}

		// Set message properties
		msg.ID = uuid.New().String()
		msg.Username = username
		msg.Timestamp = time.Now()

		// Send message to all clients
		broadcast <- msg
	}
}

// Handle messages broadcast to all clients
func handleMessages() {
	for {
		// Grab the next message from the broadcast channel
		msg := <-broadcast

		// Send it to every client
		for client := range clients {
			err := client.WriteJSON(msg)
			if err != nil {
				log.Printf("Error sending message: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
	}
}

// Handle file uploads to MinIO
func handleFileUpload(c *gin.Context) {
	// Get username from form
	username := c.PostForm("username")
	if username == "" {
		username = "anonymous-" + uuid.New().String()[0:8]
	}

	// Get the file from the request
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No file provided"})
		return
	}
	defer file.Close()

	// Generate a unique filename
	fileExt := filepath.Ext(header.Filename)
	objectName := fmt.Sprintf("%s-%s%s", time.Now().Format("20060102-150405"), uuid.New().String()[0:8], fileExt)

	// Upload the file to MinIO
	ctx := context.Background()
	_, err = minioClient.PutObject(ctx, bucketName, objectName, file, header.Size, minio.PutObjectOptions{
		ContentType: http.DetectContentType(make([]byte, 512)), // Detect content type
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to upload file to storage"})
		log.Printf("Error uploading file: %v", err)
		return
	}

	// Generate file URL
	fileURL := fmt.Sprintf("/download/%s", objectName)

	// Create a message with the file information
	msg := Message{
		ID:        uuid.New().String(),
		Username:  username,
		Content:   fmt.Sprintf("shared a file: %s", header.Filename),
		FileURL:   fileURL,
		FileName:  header.Filename,
		Timestamp: time.Now(),
	}

	// Broadcast the message
	broadcast <- msg

	// Return success response
	c.JSON(http.StatusOK, gin.H{
		"message":  "File uploaded successfully",
		"fileUrl":  fileURL,
		"fileName": header.Filename,
	})
}

// Handle file downloads from MinIO
func handleFileDownload(c *gin.Context) {
	filename := c.Param("filename")

	// Get object from MinIO
	ctx := context.Background()
	object, err := minioClient.GetObject(ctx, bucketName, filename, minio.GetObjectOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve file"})
		log.Printf("Error getting object: %v", err)
		return
	}
	defer object.Close()

	// Get object info
	info, err := object.Stat()
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		log.Printf("Error getting object info: %v", err)
		return
	}

	// Set headers
	c.Header("Content-Description", "File Transfer")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", info.Key))
	c.Header("Content-Type", info.ContentType)
	c.Header("Content-Length", fmt.Sprintf("%d", info.Size))

	// Stream the file to the response
	if _, err := io.Copy(c.Writer, object); err != nil {
		log.Printf("Error streaming file: %v", err)
	}
}
