package roomMapping

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/redis/go-redis/v9"
	openai "github.com/sashabaranov/go-openai"
	"golang.org/x/time/rate"
)

var (
	openaiApiKey = os.Getenv("OPENAI_API_KEY")
	redisHost    = os.Getenv("REDIS_HOST")
	redisPort    = os.Getenv("REDIS_PORT")

	redisClient  *redis.Client
	openaiClient *openai.Client

	// Rate limiter for OpenAI API
	rateLimiter = rate.NewLimiter(rate.Limit(3), 5) // 3 requests per second, burst of 5
)

func init() {
	functions.CloudEvent("room-mapping", roomMapping)

	redisClient = redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", redisHost, redisPort),
	})

	openaiClient = openai.NewClient(openaiApiKey)
}

type MessagePublishedData struct {
	Message PubSubMessage `json:"message"`
}

type PubSubMessage struct {
	Data       []byte            `json:"data"`
	Attributes map[string]string `json:"attributes"`
}

type UnmappedRoom struct {
	HotelID           string    `json:"hotelId"`
	ReferenceRooms    []RefRoom `json:"referenceRooms"`
	SupplierRateNames []string  `json:"supplierRateNames"`
}

type RefRoom struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type LLMResponse struct {
	Mappings map[string]RoomMappingResult `json:"mappings"`
}

type RoomMappingResult struct {
	Name string `json:"name"`
	ID   uint   `json:"id"`
}

func roomMapping(ctx context.Context, e event.Event) error {
	var msg MessagePublishedData
	if err := e.DataAs(&msg); err != nil {
		return fmt.Errorf("event.DataAs: %v", err)
	}

	batchKey := msg.Message.Attributes["batchKey"]
	processingId := msg.Message.Attributes["processingId"]

	if batchKey == "" {
		batchKey = "unmapped_rooms"
	}

	return processUnmappedRooms(ctx, batchKey, processingId)
}

func processUnmappedRooms(ctx context.Context, batchKey string, processingId string) error {
	// Create an idempotency key using both the batchKey and processingId
	idempotencyKey := fmt.Sprintf("%s:%s:processed", batchKey, processingId)

	// Check if this exact message has been processed before
	exists, err := redisClient.Exists(ctx, idempotencyKey).Result()
	if err != nil {
		log.Printf("Error checking idempotency key: %v", err)
	} else if exists > 0 {
		log.Printf("Message with processingId %s for batch %s already processed, skipping",
			processingId, batchKey)
		return nil
	}

	// Set the idempotency key without TTL
	redisClient.Set(ctx, idempotencyKey, "1", 0)

	data, err := redisClient.Get(ctx, batchKey).Result()
	if err == redis.Nil || data == "" {
		log.Printf("No data found for batch key: %s", batchKey)
		return nil
	} else if err != nil {
		log.Printf("Redis GET error: %v", err)
		return fmt.Errorf("redis GET error: %w", err)
	}

	log.Printf("Processing batch: %s", batchKey)

	// Track success rates and response times
	successCount := 0
	totalCount := 0
	var totalResponseTime time.Duration

	var hotels []UnmappedRoom
	if err := json.Unmarshal([]byte(data), &hotels); err != nil {
		log.Printf("Failed to unmarshal hotels data: %v", err)
		return fmt.Errorf("json.Unmarshal: %w", err)
	}

	log.Printf("Found %d hotels to process", len(hotels))

	// Set totalCount to the number of hotels
	totalCount = len(hotels)

	// Skip metrics logging if no hotels to process
	if totalCount == 0 {
		log.Printf("No hotels to process, removing batch key: %s", batchKey)
		return redisClient.Del(ctx, batchKey).Err()
	}

	// Create a worker pool with rate limiting
	maxConcurrent := 10
	rateLimiter = rate.NewLimiter(rate.Limit(10), 15)
	sem := make(chan struct{}, maxConcurrent)
	errChan := make(chan error, len(hotels))

	// Channel to collect processing results
	resultChan := make(chan time.Duration, len(hotels))

	var wg sync.WaitGroup

	for i, hotel := range hotels {
		wg.Add(1)

		go func(idx int, h UnmappedRoom) {
			defer wg.Done()

			// Acquire semaphore
			sem <- struct{}{}
			defer func() { <-sem }()

			log.Printf("Processing hotel %d/%d: %s", idx+1, len(hotels), h.HotelID)
			startTime := time.Now()
			if err := processHotel(ctx, h); err != nil {
				log.Printf("Error processing hotel %s: %v", h.HotelID, err)
				errChan <- err
			} else {
				// Record successful processing and time taken
				resultChan <- time.Since(startTime)
			}
		}(i, hotel)
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(errChan)
	close(resultChan)

	// Collect errors
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	// Collect successful results
	for duration := range resultChan {
		successCount++
		totalResponseTime += duration
	}

	if len(errs) > 0 {
		log.Printf("Encountered %d errors during processing", len(errs))
		return fmt.Errorf("encountered %d errors during processing", len(errs))
	}

	// Log metrics for future tuning - avoid division by zero
	if successCount > 0 {
		log.Printf("Processed %d hotels with %d successes (%.1f%%). Avg response time: %v",
			totalCount, successCount, float64(successCount)/float64(totalCount)*100,
			totalResponseTime/time.Duration(successCount))
	} else {
		log.Printf("Processed %d hotels with 0 successes (0.0%%). No average response time available.",
			totalCount)
	}

	log.Printf("Successfully processed all hotels, removing batch key: %s", batchKey)
	return redisClient.Del(ctx, batchKey).Err()
}

func processHotel(ctx context.Context, hotel UnmappedRoom) error {
	log.Printf("Getting existing mappings for hotel: %s", hotel.HotelID)
	existing, err := getExistingMappings(ctx, hotel.HotelID)
	if err != nil {
		log.Printf("Error getting existing mappings: %v", err)
		return err
	}

	var unmapped []string
	for _, s := range hotel.SupplierRateNames {
		if _, exists := existing[s]; !exists {
			unmapped = append(unmapped, s)
		}
	}

	log.Printf("Found %d unmapped supplier rate names for hotel %s", len(unmapped), hotel.HotelID)
	if len(unmapped) == 0 {
		log.Printf("No unmapped supplier rate names for hotel %s", hotel.HotelID)
		return nil
	}

	prompt := createPrompt(hotel.HotelID, hotel.ReferenceRooms, unmapped)
	log.Printf("Created prompt for hotel %s with %d reference rooms and %d unmapped rates",
		hotel.HotelID, len(hotel.ReferenceRooms), len(unmapped))

	llmResp, err := callGPTForMapping(ctx, prompt)
	if err != nil {
		log.Printf("Error calling GPT for mapping: %v", err)
		return err
	}

	log.Printf("Received mapping response with %d mappings", len(llmResp.Mappings))

	return storeMappings(ctx, hotel.HotelID, llmResp.Mappings)
}

func getExistingMappings(ctx context.Context, hotelID string) (map[string]RoomMappingResult, error) {
	key := fmt.Sprintf("room_mapping:%s", hotelID)
	data, err := redisClient.Get(ctx, key).Result()
	if err == redis.Nil || data == "" {
		return make(map[string]RoomMappingResult), nil
	} else if err != nil {
		return nil, err
	}

	var mappings map[string]RoomMappingResult
	if err := json.Unmarshal([]byte(data), &mappings); err != nil {
		redisClient.Del(ctx, key)
		return make(map[string]RoomMappingResult), nil
	}

	return mappings, nil
}

func storeMappings(ctx context.Context, hotelID string, newMappings map[string]RoomMappingResult) error {
	// Create the key for the hash that stores all mappings for this hotel
	hashKey := fmt.Sprintf("room_mapping:%s", hotelID)

	if len(newMappings) == 0 {
		return nil
	}

	// Get existing mappings
	existing, err := getExistingMappings(ctx, hotelID)
	if err != nil {
		log.Printf("Error getting existing mappings: %v", err)
		// Continue with just the new mappings
		existing = make(map[string]RoomMappingResult)
	}

	log.Printf("Found %d existing mappings for hotel %s", len(existing), hotelID)

	// Merge new mappings with existing ones
	finalMappings := make(map[string]RoomMappingResult)

	// Copy existing mappings first
	for k, v := range existing {
		finalMappings[k] = v
	}

	// Then add new mappings (overwriting any existing ones)
	for k, v := range newMappings {
		finalMappings[k] = v
	}

	// Normalize all keys in the mappings
	// Note: Implementing normalizeRoomName function as it was in the service version
	normalizedMappings := make(map[string]RoomMappingResult)
	for supplierName, result := range finalMappings {
		normalizedName := normalizeRoomName(supplierName)
		normalizedMappings[normalizedName] = result
	}
	finalMappings = normalizedMappings

	// Marshal the final mappings map to JSON
	finalJSON, err := json.Marshal(finalMappings)
	if err != nil {
		log.Printf("Failed to marshal mappings for hotel %s: %v", hotelID, err)
		return fmt.Errorf("failed to marshal mappings: %w", err)
	}

	// Store the JSON string in Redis without expiration
	err = redisClient.Set(ctx, hashKey, string(finalJSON), 0).Err()
	if err != nil {
		log.Printf("Failed to store mappings in Redis for hotel %s: %v", hotelID, err)
		return err
	}

	log.Printf("Stored %d mappings for hotel %s", len(finalMappings), hotelID)
	return nil
}

func createPrompt(hotelID string, refRooms []RefRoom, supplierNames []string) string {
	var formattedReferenceRooms strings.Builder
	for _, room := range refRooms {
		formattedReferenceRooms.WriteString(fmt.Sprintf("- ID: %d, Name: %s\n", room.ID, room.Name))
	}

	supplierRates := strings.Join(supplierNames, "\n- ")

	return fmt.Sprintf(`You are a hotel room mapping expert. Your task is to match supplier rate names to reference room names based on their descriptions.

HOTEL ID: %s

REFERENCE ROOMS:
%s

SUPPLIER RATE NAMES:
- %s

For each supplier rate name, find the most appropriate matching reference room. Consider room types, bed configurations, occupancy, and amenities in your matching. If a reference room name contains a bedding type, please use it in your matching.

Return your response as a JSON object with the following structure:
{
  "mappings": {
    "supplier_rate_name_1": {
      "name": "matching_reference_room_name_1",
      "id": 123
    },
    "supplier_rate_name_2": {
      "name": "matching_reference_room_name_2",
      "id": 456
    },
    ...
  }
}

Don't edit the supplier rate name in any way, don't normalize it. only map them to the reference rooms. supplier_rate_name_1 is only an example, it should be replaced with the actual supplier rate names.
Only include mappings where you are confident. If you cannot find a good match for a supplier rate, return it as "not_mapped" with an Id of 0

IMPORTANT: Your response MUST be a valid JSON object and nothing else. Do not include any explanations or text outside of the JSON.`,
		hotelID,
		formattedReferenceRooms.String(),
		supplierRates)
}

func callGPTForMapping(ctx context.Context, prompt string) (LLMResponse, error) {

	//start time
	startTime := time.Now()
	// Calculate timeout based on prompt size - increase the base timeout
	baseTimeout := 60 * time.Second                                 // Increased from 30 to 60 seconds
	promptLenFactor := time.Duration(len(prompt)/500) * time.Second // More time for longer prompts
	timeout := baseTimeout + promptLenFactor

	log.Printf("Setting timeout of %v for prompt of length %d", timeout, len(prompt))

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Printf("Sending request to OpenAI API")

	// Add retry logic
	maxRetries := 3
	var resp openai.ChatCompletionResponse
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Create a new context with timeout for each attempt
		attemptCtx, attemptCancel := context.WithTimeout(ctx, timeout)

		resp, err = openaiClient.CreateChatCompletion(attemptCtx, openai.ChatCompletionRequest{
			Model: "gpt-4o",
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: "You are a hotel room mapping assistant that matches supplier rate names to reference room names.",
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: prompt,
				},
			},
			Temperature: 0.1,
		})

		attemptCancel() // Cancel the attempt context

		if err == nil && len(resp.Choices) > 0 {
			log.Printf("Received successful response from OpenAI API")
			break
		}

		log.Printf("OpenAI API attempt %d failed: %v", attempt, err)

		if attempt < maxRetries {
			// Calculate backoff with jitter
			baseDelay := time.Duration(attempt*3) * time.Second
			jitter := time.Duration(rand.Intn(3)) * time.Second
			backoff := baseDelay + jitter

			log.Printf("Retrying in %v...", backoff)
			time.Sleep(backoff)
		}
	}

	if err != nil {
		log.Printf("OpenAI API error after %d attempts: %v", maxRetries, err)
		return LLMResponse{}, fmt.Errorf("OpenAI API error after %d attempts: %w", maxRetries, err)
	}

	if len(resp.Choices) == 0 {
		log.Printf("OpenAI API returned no choices")
		return LLMResponse{}, fmt.Errorf("OpenAI API returned no choices")
	}

	content := resp.Choices[0].Message.Content
	//log time per request
	log.Printf("Time taken to get response from OpenAI: %v", time.Since(startTime))
	return parseGPTResponse(content)
}

func parseGPTResponse(raw string) (LLMResponse, error) {
	var res LLMResponse = LLMResponse{
		Mappings: make(map[string]RoomMappingResult),
	}

	// Extract JSON from the response
	jsonStart := strings.Index(raw, "{")
	jsonEnd := strings.LastIndex(raw, "}")

	if jsonStart != -1 && jsonEnd != -1 && jsonEnd > jsonStart {
		jsonStr := raw[jsonStart : jsonEnd+1]

		err := json.Unmarshal([]byte(jsonStr), &res)
		if err == nil {
			log.Printf("Successfully parsed JSON response with %d mappings", len(res.Mappings))
			return res, nil
		}

		log.Printf("Failed to parse JSON: %v", err)
	} else {
		log.Printf("No valid JSON found in response")
	}

	// If we get here, JSON parsing failed
	log.Printf("Failed to parse response as JSON, trying alternative formats")

	// Try to parse the text-based formats
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		log.Printf("Processing line: %s", line)

		// Format 1: "supplier -> ID: X, Name: Y"
		if strings.Contains(line, "->") {
			parts := strings.Split(line, "->")
			if len(parts) != 2 {
				continue
			}

			supplierName := strings.TrimSpace(parts[0])
			refRoomPart := strings.TrimSpace(parts[1])

			// Extract ID and Name using regex
			idMatch := regexp.MustCompile(`ID: (\d+)`).FindStringSubmatch(refRoomPart)
			nameMatch := regexp.MustCompile(`Name: (.+)$`).FindStringSubmatch(refRoomPart)

			if len(idMatch) > 1 && len(nameMatch) > 1 {
				id, err := strconv.ParseUint(idMatch[1], 10, 32)
				if err != nil {
					log.Printf("Failed to parse ID: %v", err)
					continue
				}

				name := strings.TrimSpace(nameMatch[1])
				res.Mappings[supplierName] = RoomMappingResult{
					ID:   uint(id),
					Name: name,
				}
				log.Printf("Parsed mapping: %s -> %s (ID: %d)", supplierName, name, id)
			}
		} else if strings.Contains(line, "matches with Reference Room") { // Format 2: "'supplier' matches with Reference Room 'name' (ID: X)"
			// Extract supplier name
			supplierMatch := regexp.MustCompile(`['"](.+?)['"]`).FindStringSubmatch(line)
			if len(supplierMatch) < 2 {
				log.Printf("Failed to extract supplier name from line")
				continue
			}
			supplierName := supplierMatch[1]

			// Extract reference room name
			roomNameMatch := regexp.MustCompile(`Reference Room ['"](.+?)['"]`).FindStringSubmatch(line)
			if len(roomNameMatch) < 2 {
				log.Printf("Failed to extract room name from line")
				continue
			}
			roomName := roomNameMatch[1]

			// Extract ID
			idMatch := regexp.MustCompile(`\(ID: (\d+)\)`).FindStringSubmatch(line)
			if len(idMatch) < 2 {
				log.Printf("Failed to extract ID from line")
				continue
			}

			id, err := strconv.ParseUint(idMatch[1], 10, 32)
			if err != nil {
				log.Printf("Failed to parse ID: %v", err)
				continue
			}

			res.Mappings[supplierName] = RoomMappingResult{
				ID:   uint(id),
				Name: roomName,
			}
			log.Printf("Parsed mapping: %s -> %s (ID: %d)", supplierName, roomName, id)
		}
	}

	if len(res.Mappings) == 0 {
		log.Printf("Failed to parse LLM response, no mappings found")
		return res, fmt.Errorf("failed to parse LLM response, no mappings found: %s", raw)
	}
	return res, nil
}

// Helper function to normalize room names
func normalizeRoomName(name string) string {
	// Convert to lowercase and trim spaces
	normalized := strings.ToLower(strings.TrimSpace(name))

	// Replace multiple spaces with a single space
	normalized = regexp.MustCompile(`\s+`).ReplaceAllString(normalized, " ")

	// Remove common punctuation that doesn't affect meaning
	normalized = strings.ReplaceAll(normalized, "-", " ")
	normalized = strings.ReplaceAll(normalized, ",", " ")
	normalized = strings.ReplaceAll(normalized, ".", " ")
	normalized = strings.ReplaceAll(normalized, "/", " ")
	normalized = strings.ReplaceAll(normalized, "(", " ")
	normalized = strings.ReplaceAll(normalized, ")", " ")

	// Clean up any resulting multiple spaces again
	normalized = regexp.MustCompile(`\s+`).ReplaceAllString(normalized, " ")
	normalized = strings.TrimSpace(normalized)

	return normalized
}
