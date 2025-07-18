# Room Mapper Async Cloud Function

A Google Cloud Function that processes hotel room mappings asynchronously using OpenAI's GPT-4 model. This function matches supplier rate names to reference room names for hotels.

## Overview

This cloud function:
- Processes batches of unmapped hotel rooms
- Uses OpenAI GPT-4 to intelligently match supplier rate names to reference room names
- Stores mappings in Redis for persistence
- Implements rate limiting and retry logic for API calls
- Supports concurrent processing with worker pools

## Architecture

- **Trigger**: Cloud Events (Pub/Sub messages)
- **Processing**: Concurrent processing with rate limiting
- **Storage**: Redis for mapping persistence
- **AI**: OpenAI GPT-4 for intelligent room matching
- **Monitoring**: Comprehensive logging and metrics

## Prerequisites

- Google Cloud Platform account
- Redis instance (Cloud Memorystore or external)
- OpenAI API key
- Go 1.21+

## Environment Variables

The following environment variables must be set:

```bash
OPENAI_API_KEY=your_openai_api_key
REDIS_HOST=your_redis_host
REDIS_PORT=your_redis_port
REDIS_USER=your_redis_username
REDIS_PASSWORD=your_redis_password
```

## Local Development

1. **Install dependencies**:
   ```bash
   go mod tidy
   ```

2. **Set environment variables**:
   ```bash
   export OPENAI_API_KEY="your_key"
   export REDIS_HOST="localhost"
   export REDIS_PORT="6379"
   export REDIS_USER=""
   export REDIS_PASSWORD=""
   ```

3. **Run locally** (requires Functions Framework):
   ```bash
   go run .
   ```

## Deployment

### Option 1: Google Cloud Functions (2nd gen)

```bash
gcloud functions deploy room-mapper-async \
  --gen2 \
  --runtime=go121 \
  --region=us-central1 \
  --source=. \
  --entry-point=roomMapping \
  --trigger-topic=room-mapping-topic \
  --set-env-vars=OPENAI_API_KEY=your_key,REDIS_HOST=your_host,REDIS_PORT=6379,REDIS_USER=your_user,REDIS_PASSWORD=your_password \
  --memory=2GB \
  --timeout=540s
```

### Option 2: Using Docker

1. **Build the container**:
   ```bash
   docker build -t room-mapper-async .
   ```

2. **Run locally**:
   ```bash
   docker run -p 8080:8080 \
     -e OPENAI_API_KEY=your_key \
     -e REDIS_HOST=your_host \
     -e REDIS_PORT=6379 \
     -e REDIS_USER=your_user \
     -e REDIS_PASSWORD=your_password \
     room-mapper-async
   ```

## Input Format

The function expects Pub/Sub messages with the following structure:

```json
{
  "message": {
    "data": "base64_encoded_data",
    "attributes": {
      "batchKey": "unmapped_rooms_batch_123",
      "processingId": "unique_processing_id"
    }
  }
}
```

The `data` field should contain a JSON array of `UnmappedRoom` objects:

```json
[
  {
    "hotelId": "hotel_123",
    "referenceRooms": [
      {
        "id": 1,
        "name": "Standard Double Room"
      }
    ],
    "supplierRateNames": [
      "Standard Double Room - King Bed",
      "Deluxe Room - Queen Bed"
    ]
  }
]
```

## Output

The function stores mappings in Redis with the key pattern: `room_mapping:{hotelId}`

Each mapping contains:
- Supplier rate name (normalized)
- Reference room name
- Reference room ID

## Error Handling

- **Idempotency**: Uses Redis to prevent duplicate processing
- **Retry Logic**: Implements exponential backoff for OpenAI API calls
- **Rate Limiting**: Prevents API quota exhaustion
- **Graceful Degradation**: Continues processing even if some hotels fail

## Monitoring

The function logs:
- Processing metrics (success rate, response times)
- Error details with context
- Performance statistics
- API call timing

## Performance

- **Concurrency**: Up to 10 concurrent hotel processing
- **Rate Limiting**: 10 requests/second to OpenAI API
- **Timeout**: Dynamic timeout based on prompt length (60s base + prompt length factor)
- **Memory**: Recommended 2GB for optimal performance

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests if applicable
5. Submit a pull request

## License

[Add your license information here] 