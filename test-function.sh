#!/bin/bash

# Test script for Room Mapper Async Cloud Function
# This script publishes a test message to the Pub/Sub topic

set -e

# Configuration
PROJECT_ID="nuitee-lite-api"
TOPIC_NAME="room-mapping-topic"
REGION="us-east1"
FUNCTION_NAME="room-mapping-async2"

# Sample test data
TEST_DATA='[
  {
    "hotelId": "test_hotel_123",
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
      "Deluxe Room - Queen Bed",
      "Standard Double Room - Twin Beds"
    ]
  }
]'

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}🧪 Testing Room Mapper Async Cloud Function...${NC}"

# Encode the test data to base64
ENCODED_DATA=$(echo "$TEST_DATA" | base64)

# Create the message JSON
MESSAGE_JSON='{
  "message": {
    "data": "'$ENCODED_DATA'",
    "attributes": {
      "batchKey": "test_batch_123",
      "processingId": "test_processing_456"
    }
  }
}'

echo -e "${YELLOW}📤 Publishing test message to topic: $TOPIC_NAME${NC}"
echo -e "${YELLOW}📋 Test data:${NC}"
echo "$TEST_DATA" | jq '.'

# Publish the message
echo "$MESSAGE_JSON" | gcloud pubsub topics publish $TOPIC_NAME --project=$PROJECT_ID --message=-

echo -e "${GREEN}✅ Test message published successfully!${NC}"
echo -e "${YELLOW}📊 Check the function logs to see the processing:${NC}"
echo -e "   gcloud functions logs read $FUNCTION_NAME --region=$REGION --project=$PROJECT_ID --limit=50" 