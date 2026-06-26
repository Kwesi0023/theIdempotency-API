## Welcome to theIdempotency-API

# The Normal way
So a normal call  
cUrl 
```
 curl.exe -X POST {{baseURL}}/process-payment -H "Idempotency-Key: payment-001" -H "Content-Type: application/json" -d '"{\"amount\": 200, \"currency\": \"GHS\"}"'
```
response: ``` {"message":"Charged 100 GHS"} ```

# The Double Charger
If there is a newtwork lag or timeout and a second request with the same idempotency_key and request body is sent to the server you would see ``X-Cache-Hit: true`` (for postman and some other testers you would see it directly)
cURL
```
    curl.exe -i -X POST {{baseURL}}/process-payment -H "Idempotency-Key: payment-001" -H "Content-Type: application/json" -d '"{\"amount\": 200, \"currency\": \"GHS\"}"'
```
to see whats normally under the hood when a request is being sent using cURL. 
you would see something like  
`` HTTP/1.1 200 OK  Content-Type: application/json X-Cache-Hit: true Date: ..Content-Length: 29``


# Mismatch Test 
When the same idempotency key is used twice but there is a mismatch in the request body (Same Key, Different Body you would get 409 Conflict)
cURL
```
    curl.exe -X POST {{baseURL}}/process-payment -H "Idempotency-Key: payment-001" -H   "Content-Type: application/json" -d "{\"amount\": 200, \"currency\": \"GHS\"}"
```
response: ``` {"error":"Idempotency key already used for a different request body."}```

# Concurrent 
open two terminals and run both commands quickly (say, within a 2 second window)
as expected you would receive the same response {"message":"Charged 100 GHS"}
the first one will be successful , the second will block moslty just be doing the same thing

# Missing Idempotency-key Header
if you do not add the idempotency key to the request being sent you would get a 400 error
cURL
```
curl.exe -X POST {{baseURL}}/process-payment -H "Content-Type: application/json" -d '"{\"amount\": 220, \"currency\": \"GHS\"}"'
```
response: `` missing Idempotency-Key header``

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant API as Idempotency Gateway
    participant DB as MySQL Database

    %% --- FIRST TRANSACTION PATH ---
    rect rgb(230, 245, 230)
        note over Client, DB: Path 1: First Transaction (Happy Path)
        Client->>API: POST /process-payment [Headers: Idempotency-Key]
        activate API
        API->>API: Read payload & compute SHA-256 hash
        API->>DB: Begin Transaction (tx.Begin())
        API->>DB: SELECT ... FOR UPDATE (Check if Key exists)
        DB-->>API: Row Not Found (gorm.ErrRecordNotFound)
        
        API->>DB: INSERT row [Status: "IN_FLIGHT", RequestHash]
        
        API->>API: Simulate downstream processing (time.Sleep 2s)
        
        API->>DB: UPDATE row [Status: "SUCCESS", ResponseCode: 200, ResponseBody]
        API->>DB: Commit Transaction (tx.Commit())
        
        API-->>Client: 200 OK {"message": "Charged 100 GHS"}
        deactivate API
    end

    %% --- DUPLICATE TRANSACTION PATH ---
    rect rgb(240, 240, 255)
        note over Client, DB: Path 2: Duplicate Attempt (Cached Replay)
        Client->>API: POST /process-payment [Same Idempotency-Key & Payload]
        activate API
        API->>API: Read payload & compute SHA-256 hash
        API->>DB: Begin Transaction (tx.Begin())
        API->>DB: SELECT ... FOR UPDATE
        DB-->>API: Row Found [Status: "SUCCESS"]
        
        alt Hashes Match (Valid Replay)
            API->>API: Validate incoming hash == stored hash
            API->>DB: Commit Transaction
            API-->>Client: 200 OK [Header: X-Cache-Hit: true] + Cached Body
        else Hashes Mismatch (Fraud/Payload Tampering)
            API->>API: Validate incoming hash != stored hash
            API->>DB: Commit Transaction
            API-->>Client: 409 Conflict {"error": "Idempotency key already used..."}
        end
        deactivate API
    end

    %% --- CONCURRENT IN-FLIGHT PATH ---
    rect rgb(255, 240, 230)
        note over Client, DB: Path 3: Concurrent Request (In-Flight Block)
        Client->>API: Request B arrives (Same Key & Payload while A is mid-flight)
        activate API
        API->>API: Read payload & compute SHA-256 hash
        API->>DB: Begin Transaction (tx.Begin())
        API->>DB: SELECT ... FOR UPDATE
        
        DB-->>API: Returns Row [Initially captured state: IN_FLIGHT]
        
        API->>DB: Re-fetch row (tx.First()) to grab freshly committed updates
        DB-->>API: Returns Updated Row [Status: "SUCCESS"]
        
        API->>API: Validate incoming hash == stored hash
        API->>DB: Commit Transaction
        API-->>Client: 200 OK [Header: X-Cache-Hit: true] + Cached Body
        deactivate API
    end