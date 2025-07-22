# Room Mapper Async Cloud Function

A Google Cloud Function that processes room mapping requests asynchronously using Redis for job storage and OpenAI GPT-4 for intelligent room name matching.

## Features

- **Asynchronous Processing**: Uses Redis to store batch jobs and process them asynchronously
- **Intelligent Room Mapping**: Leverages OpenAI GPT-4 to match supplier rate names to reference room names
- **Idempotency**: Prevents duplicate processing using Redis-based idempotency keys
- **Rate Limiting**: Implements rate limiting for OpenAI API calls
- **Error Handling**: Robust error handling with retry logic
- **Metrics**: Tracks success rates and response times

## Architecture

1. **Pub/Sub Trigger**: Function is triggered by Pub/Sub messages
2. **Redis Storage**: Batch data is stored in Redis with a `batchKey`
3. **Processing**: Function retrieves data from Redis and processes each hotel
4. **OpenAI Integration**: Uses GPT-4 to map supplier rate names to reference rooms
5. **Results Storage**: Mappings are stored back in Redis for each hotel

## Environment Variables

Create a `.env` file with the following variables:

```bash
OPENAI_API_KEY=your_openai_api_key_here
REDIS_HOST=localhost
REDIS_PORT=6379
```

## Local Testing

### Prerequisites

1. **Redis**: Start Redis locally
   ```bash
   docker run -d -p 6379:6379 redis:alpine
   ```

2. **Environment**: Create `.env` file with required credentials

### Test the Function

1. **Setup test data in Redis**:
   ```bash
   ./test-local.sh
   ```

2. **Deploy and test with Cloud Function**:
   ```bash
   # Deploy to Google Cloud
   gcloud functions deploy room-mapping-async2 \
     --gen2 \
     --runtime=go121 \
     --region=us-east1 \
     --source=. \
     --entry-point=roomMapping \
     --trigger-topic=room-mapping-topic \
     --project=nuitee-lite-api \
     --set-env-vars=OPENAI_API_KEY=your_key,REDIS_HOST=your_redis_host,REDIS_PORT=6379
   ```

3. **Test with the cloud function**:
   ```bash
   ./test-function.sh
   ```

### Test Data Structure

The function expects data in this format:

```json
[
  {
    "hotelId": "hotel_123",
    "referenceRooms": [
      {
        "id": 1,
        "name": "Standard Double Room"
      },
      {
        "id": 2,
        "name": "Deluxe Room"
      }
    ],
    "supplierRateNames": [
      "Standard Double Room - King Bed",
      "Deluxe Room - Queen Bed"
    ]
  }
]
```

## Redis Keys

- **Batch Data**: `{batchKey}` - Contains the JSON array of hotels to process
- **Idempotency**: `{batchKey}:{processingId}:processed` - Prevents duplicate processing
- **Mappings**: `room_mapping:{hotelId}` - Stores the final room mappings for each hotel

## Development

### Running Locally

1. Start Redis
2. Set up environment variables
3. Use the test scripts to simulate the function

### Deployment

```bash
gcloud functions deploy room-mapping-async2 \
  --gen2 \
  --runtime=go121 \
  --region=us-east1 \
  --source=. \
  --entry-point=roomMapping \
  --trigger-topic=room-mapping-topic \
  --project=nuitee-lite-api \
  --set-env-vars=OPENAI_API_KEY=your_key,REDIS_HOST=your_redis_host,REDIS_PORT=6379
```

## Monitoring

Check function logs:
```bash
gcloud functions logs read room-mapping-async2 --region=us-east1 --project=nuitee-lite-api --limit=50
```

## Testing Scripts

- `test-function.sh`: Tests the deployed cloud function
- `test-local.sh`: Sets up test data in Redis for local testing 