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
	openaiApiKey  = os.Getenv("OPENAI_API_KEY")
	redisHost     = os.Getenv("REDIS_HOST")
	redisPort     = os.Getenv("REDIS_PORT")
	redisUser     = os.Getenv("REDIS_USER")
	redisPassword = os.Getenv("REDIS_PASSWORD")
	redisDB       = 0

	redisClient  *redis.Client
	openaiClient *openai.Client

	// Rate limiter for OpenAI API
	rateLimiter = rate.NewLimiter(rate.Limit(3), 5) // 3 requests per second, burst of 5
)

func init() {
	functions.CloudEvent("room-mapping", roomMapping)

	// Add debug logging for Redis connection
	log.Printf("Cloud Function Redis Connection - Host: %s, Port: %s, User: %s", redisHost, redisPort, redisUser)

	redisClient = redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", redisHost, redisPort),
		Username: redisUser,
		Password: redisPassword,
		DB:       redisDB,
	})

	// Test Redis connection and log the result
	ctx := context.Background()
	err := redisClient.Ping(ctx).Err()
	if err != nil {
		log.Printf("ERROR: Failed to connect to Redis: %v", err)
	} else {
		log.Printf("SUCCESS: Connected to Redis at %s:%s", redisHost, redisPort)
	}

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
	Mappings map[string]RoomMappingResult `json:"mappings,omitempty"`
}

type RoomMappingResult struct {
	Name           string  `json:"name"`
	ID             uint    `json:"id"`
	Confidence     float64 `json:"confidence"`
	MatchRationale string  `json:"match_rationale"`
}

// UnmarshalJSON allows RoomMappingResult.ID to be unmarshalled from either a string or a number
func (r *RoomMappingResult) UnmarshalJSON(data []byte) error {
	type Alias RoomMappingResult
	aux := &struct {
		ID interface{} `json:"id"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	// Handle ID as string or number
	switch v := aux.ID.(type) {
	case string:
		id, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return err
		}
		r.ID = uint(id)
	case float64:
		r.ID = uint(v)
	case int:
		r.ID = uint(v)
	case nil:
		r.ID = 0
	default:
		return fmt.Errorf("unexpected type for id: %T", v)
	}
	return nil
}

func roomMapping(ctx context.Context, e event.Event) error {
	var msg MessagePublishedData
	if err := e.DataAs(&msg); err != nil {
		return fmt.Errorf("event.DataAs: %v", err)
	}

	batchKey := msg.Message.Attributes["batchKey"]
	processingId := msg.Message.Attributes["processingId"]

	// Add debug logging
	log.Printf("Received message - batchKey: '%s', processingId: '%s'", batchKey, processingId)
	log.Printf("Message attributes: %+v", msg.Message.Attributes)
	log.Printf("Message data: %s", string(msg.Message.Data))

	// Try to extract from message data if attributes are empty
	if batchKey == "" || processingId == "" {
		var messageData map[string]interface{}
		if err := json.Unmarshal(msg.Message.Data, &messageData); err == nil {
			if batchKey == "" {
				if val, ok := messageData["batchKey"].(string); ok {
					batchKey = val
					log.Printf("Extracted batchKey from message data: '%s'", batchKey)
				}
			}
			if processingId == "" {
				if val, ok := messageData["processingId"].(string); ok {
					processingId = val
					log.Printf("Extracted processingId from message data: '%s'", processingId)
				}
			}
		}
	}

	if batchKey == "" {
		batchKey = "unmapped_rooms"
		log.Printf("Using fallback batchKey: '%s'", batchKey)
	}

	return processUnmappedRooms(ctx, batchKey, processingId)
}

// 1. Add Lua script for atomic pre-check and lock
const roomMappingLua = `
local todo = {}
for i = 1, #ARGV do
  local name = ARGV[i]
  if redis.call('HEXISTS', KEYS[1], name) == 0 and
     redis.call('SISMEMBER', KEYS[2], name) == 0 then
    table.insert(todo, name)
  end
end
if #todo == 0 then
  return 0
end
redis.call('SADD', KEYS[2], unpack(todo))
redis.call('EXPIRE', KEYS[2], 300)
return todo
`

func processUnmappedRooms(ctx context.Context, batchKey string, processingId string) error {
	data, err := redisClient.Get(ctx, batchKey).Result()
	log.Printf("Redis GET operation for key '%s' - Result: %v, Error: %v", batchKey, data != "", err)
	if err == redis.Nil || data == "" {
		log.Printf("No data found for batch key: %s", batchKey)
		return nil
	} else if err != nil {
		log.Printf("Redis GET error: %v", err)
		return fmt.Errorf("redis GET error: %w", err)
	}
	log.Printf("Processing batch: %s", batchKey)
	var hotels []UnmappedRoom
	if err := json.Unmarshal([]byte(data), &hotels); err != nil {
		log.Printf("Failed to unmarshal hotels data: %v", err)
		return fmt.Errorf("json.Unmarshal: %w", err)
	}
	log.Printf("Found %d hotels to process", len(hotels))
	if len(hotels) == 0 {
		log.Printf("No hotels to process, removing batch key: %s", batchKey)
		return redisClient.Del(ctx, batchKey).Err()
	}
	maxConcurrent := 10
	rateLimiter = rate.NewLimiter(rate.Limit(10), 15)
	sem := make(chan struct{}, maxConcurrent)
	errChan := make(chan error, len(hotels))
	resultChan := make(chan time.Duration, len(hotels))
	var wg sync.WaitGroup
	for i, hotel := range hotels {
		wg.Add(1)
		go func(idx int, h UnmappedRoom) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			log.Printf("Processing hotel %d/%d: %s", idx+1, len(hotels), h.HotelID)
			startTime := time.Now()
			if err := processHotel(ctx, h); err != nil {
				log.Printf("Error processing hotel %s: %v", h.HotelID, err)
				errChan <- err
			} else {
				resultChan <- time.Since(startTime)
			}
		}(i, hotel)
	}
	wg.Wait()
	close(errChan)
	close(resultChan)
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}
	successCount := 0
	totalResponseTime := time.Duration(0)
	for duration := range resultChan {
		successCount++
		totalResponseTime += duration
	}
	if len(errs) > 0 {
		log.Printf("Encountered %d errors during processing", len(errs))
		return fmt.Errorf("encountered %d errors during processing", len(errs))
	}
	if successCount > 0 {
		log.Printf("Processed %d hotels with %d successes (%.1f%%). Avg response time: %v", len(hotels), successCount, float64(successCount)/float64(len(hotels))*100, totalResponseTime/time.Duration(successCount))
	} else {
		log.Printf("Processed %d hotels with 0 successes (0.0%%). No average response time available.", len(hotels))
	}
	log.Printf("Successfully processed all hotels, removing batch key: %s", batchKey)
	pipe := redisClient.Pipeline()
	pipe.Del(ctx, batchKey)
	_, err = pipe.Exec(ctx)
	if err != nil {
		log.Printf("Warning: Failed to clean up Redis batch key: %v", err)
	} else {
		log.Printf("Successfully cleaned up batch key")
	}
	return nil
}

// Replace getExistingMappings with HGETALL logic
func getExistingMappings(ctx context.Context, hotelID string) (map[string]RoomMappingResult, error) {
	hashKey := fmt.Sprintf("room_map:%s", hotelID)
	mappings := make(map[string]RoomMappingResult)
	fields, err := redisClient.HGetAll(ctx, hashKey).Result()
	if err != nil {
		return nil, err
	}
	for k, v := range fields {
		var res RoomMappingResult
		if err := json.Unmarshal([]byte(v), &res); err == nil {
			mappings[k] = res
		}
	}
	return mappings, nil
}

// Remove storeMappings, replace with merge logic after GPT call
func processHotel(ctx context.Context, hotel UnmappedRoom) error {
	log.Printf("Getting existing mappings for hotel: %s", hotel.HotelID)
	redisHost := os.Getenv("REDIS_HOST")
	redisPort := os.Getenv("REDIS_PORT")
	log.Printf("Processing hotel %s - Redis connection: %s:%s", hotel.HotelID, redisHost, redisPort)

	normNames := make([]string, 0, len(hotel.SupplierRateNames))
	for _, s := range hotel.SupplierRateNames {
		normNames = append(normNames, normalizeRoomName(s))
	}
	hashKey := fmt.Sprintf("room_map:%s", hotel.HotelID)
	inflightKey := fmt.Sprintf("room_map_inflight:%s", hotel.HotelID)

	// Run Lua script to get names to map
	var toMap []string
	res, err := redisClient.Eval(ctx, roomMappingLua, []string{hashKey, inflightKey}, normNames).Result()
	if err != nil {
		log.Printf("Lua script error: %v", err)
		return err
	}
	if resInt, ok := res.(int64); ok && resInt == 0 {
		log.Printf("All supplier names already mapped or in-flight for hotel %s", hotel.HotelID)
		return nil
	}
	if arr, ok := res.([]interface{}); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				toMap = append(toMap, s)
			}
		}
	}
	if len(toMap) == 0 {
		log.Printf("All supplier names already mapped or in-flight for hotel %s", hotel.HotelID)
		return nil
	}
	log.Printf("Supplier names to map for hotel %s: %v", hotel.HotelID, toMap)

	// Prepare prompt for only the names to map
	toMapOriginal := make([]string, 0, len(toMap))
	for _, norm := range toMap {
		for _, orig := range hotel.SupplierRateNames {
			if normalizeRoomName(orig) == norm {
				toMapOriginal = append(toMapOriginal, orig)
				break
			}
		}
	}
	prompt := createPrompt(hotel.HotelID, hotel.ReferenceRooms, toMapOriginal)
	log.Printf("Created prompt for hotel %s with %d reference rooms and %d unmapped rates", hotel.HotelID, len(hotel.ReferenceRooms), len(toMapOriginal))

	startTime := time.Now()
	llmResp, err := callGPTForMapping(ctx, prompt)
	gptLatency := time.Since(startTime)
	if err != nil {
		log.Printf("Error calling GPT for mapping: %v", err)
		// clear inflight set for these names so another worker can retry
		redisClient.SRem(ctx, inflightKey, toMap)
		return err
	}
	log.Printf("Received mapping response with %d mappings", len(llmResp.Mappings))

	// Merge new mappings into hash and unlock inflight
	pipe := redisClient.TxPipeline()
	newCount := 0
	for norm, res := range llmResp.Mappings {
		val, _ := json.Marshal(res)
		pipe.HSet(ctx, hashKey, normalizeRoomName(norm), val)
		pipe.SRem(ctx, inflightKey, norm)
		newCount++
	}
	_, err = pipe.Exec(ctx)
	if err != nil {
		log.Printf("Failed to merge mappings for hotel %s: %v", hotel.HotelID, err)
		return err
	}
	log.Printf("Merged %d new mappings for hotel %s. GPT latency: %v", newCount, hotel.HotelID, gptLatency)
	return nil
}

func callGPTForMapping(ctx context.Context, prompt string) (LLMResponse, error) {

	//start time
	startTime := time.Now()
	// Calculate timeout based on prompt size - increase the base timeout
	baseTimeout := 120 * time.Second                                // Increased from 60 to 120 seconds (2 minutes)
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
			Model: "gpt-4.1",
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

	// First try to extract JSON from <json> tags
	jsonStart := strings.Index(raw, "<json>")
	jsonEnd := strings.LastIndex(raw, "</json>")

	if jsonStart != -1 && jsonEnd != -1 && jsonEnd > jsonStart {
		// Extract content between <json> tags
		jsonContent := raw[jsonStart+6 : jsonEnd] // +6 to skip "<json>"
		jsonContent = strings.TrimSpace(jsonContent)

		// Try to parse the JSON content
		err := json.Unmarshal([]byte(jsonContent), &res.Mappings)
		if err == nil {
			log.Printf("Successfully parsed JSON response with %d mappings from <json> tags", len(res.Mappings))
			return res, nil
		}

		log.Printf("Failed to parse JSON from <json> tags: %v", err)
	}

	// Fallback: Extract JSON from the response (old method)
	jsonStart = strings.Index(raw, "{")
	jsonEnd = strings.LastIndex(raw, "}")

	if jsonStart != -1 && jsonEnd != -1 && jsonEnd > jsonStart {
		jsonStr := raw[jsonStart : jsonEnd+1]

		err := json.Unmarshal([]byte(jsonStr), &res.Mappings)
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

func createPrompt(hotelID string, refRooms []RefRoom, supplierNames []string) string {
	// Format reference rooms as JSON for better structure
	refsJSON, _ := json.Marshal(refRooms)
	supplierRoomsJSON, _ := json.Marshal(supplierNames)

	return fmt.Sprintf(`You are an expert in precise hotel room mapping with deep knowledge of room nomenclature, rate plan logic, and attribute compatibility. Your goal is to rigorously match supplier rate names to reference room names, using strict semantic and structural criteria.

MATCHING PRINCIPLES:

1. THE REFERENCE CATALOG IS THE GROUND TRUTH:
   - If the reference catalog contains only one room of a given type (e.g., only one 'Two-Bedroom Villa with Private Pool'), you should map supplier rates for 'Two-Bedroom Villa' to this reference, even if the supplier name omits some attributes (like 'private pool'), as long as there is no obvious or explicit conflict.
   - Only refuse to map if the supplier name clearly contradicts a required attribute in the reference (e.g., supplier says 'no pool' but the reference has a pool).

2. ROOM NAME & TYPE MATCHING:
   - Match only if semantic or exact equivalence exists (e.g., "King Room" = "Room with 1 King Bed" = "King Bed Room").
   - Room types like "King", "Twin", "Double" must be preserved; no size inference is allowed.
   - Do NOT match if a unique room name (e.g., "Presidential Suite", "Skyline View Deluxe") is present in the reference but absent in the supplier name.

3. ROOM CLASS & STRUCTURE:
   - Match Standard, Deluxe, Premium, etc., only if they align.
   - Default to "Standard" if class is missing on either side.
   - Do NOT match a generic "Room" to a "Suite", "Villa", or "Penthouse".

4. RATE PLAN & CODE:
   - Rate plan codes must match exactly if present in both reference and supplier names.
   - Missing rate code in reference is acceptable; prioritize supplier code presence.

5. ATTRIBUTE COMPATIBILITY:
   - ALL critical attributes in the reference (view, smoking, bed config) must be present or clearly compatible in the supplier name.
   - Accessibility features (e.g., "Hearing Accessible") require exact match.
   - Extra supplier attributes are allowed if not in conflict.
   - If the supplier omits an attribute that is present in the only available reference room, assume compatibility unless there is a clear contradiction.

6. BROAD REFERENCE ROOMS:
   - If the reference room catalog provides only broad or generic room types (e.g., "Superior Double Room", "Deluxe Triple Room"), then supplier rates that are semantically equivalent or only add compatible, non-conflicting details (such as bed configuration, non-smoking, etc.) SHOULD be mapped to the broad reference room.
   - Do NOT reject a mapping just because the supplier rate includes extra details (e.g., "Superior Double Room, 1 Queen Bed, NonSmoking" should map to "Superior Double Room" if there is no conflict).
   - Only leave a supplier rate as not_mapped if it is ambiguous, introduces a clear conflict, or is for a fundamentally different room type than any reference room.
   - EXAMPLES:
     - Reference: "Superior Double Room"; Supplier: "Superior Double Room, 1 Queen Bed, NonSmoking" => MAP
     - Reference: "Deluxe Triple Room"; Supplier: "Deluxe Triple Room (1 Queen Bed and 1 Twin Bed)" => MAP
     - Reference: "Superior Twin Room"; Supplier: "Superior Twin Room, 2 Twin Beds, NonSmoking" => MAP
     - Reference: "Superior Double Room"; Supplier: "Room SUPERIOR TWO BEDROOMS" => NOT_MAPPED (if no two-bedroom reference exists)

7. MISSING DATA CASES:
   - If the supplier rate has NO room name, consider it a Standard Room (assume minimum class/attributes).

8. CONFIDENCE THRESHOLD:
   - Only return mappings with ≥ 0.80 confidence.
   - If unsure or incomplete data, default to:
     {
       "name": "not_mapped",
       "id": 0,
       "confidence": 0.0,
       "match_rationale": "Insufficient or conflicting attributes for reliable match"
     }

REFERENCE ROOM NAMES:
%s

SUPPLIER RATE NAMES:
%s

Your task is to generate an accurate mapping between supplier rate names and reference room names, using the above logic.

IMPORTANT: Wrap your JSON response in <json> tags like this:
<json>
{
    "supplier_rate_name_1": {
      "name": "matching_reference_room_name_1",
      "id": 123,
      "confidence": 0.85,
      "match_rationale": "Explanation of why this match meets all criteria"
    },
    "supplier_rate_name_2": {
      "name": "not_mapped",
      "id": 0,
      "confidence": 0.0,
      "match_rationale": "Reason for rejection"
    }
}
</json>

Rules:
- Keep supplier rate names unchanged
- Avoid assumptions or liberal interpretation
- Justify every match with clear rationale
- ALWAYS wrap your JSON response in <json> tags`, refsJSON, supplierRoomsJSON)
}
