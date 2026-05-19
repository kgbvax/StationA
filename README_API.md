# ubctrl Web API Documentation

This document describes the RESTful web service endpoints provided by the ubctrl web interface.

## Base URL

All endpoints are relative to the server root (e.g., `http://localhost:8080`).

---

## Endpoints

### 1. Get Status
- **URL:** `/api/status`
- **Method:** `GET`
- **Description:** Returns the current controller state as JSON.
- **Response:**
  - `200 OK` with JSON body:
    ```json
    {
      "frequency_khz": 14000,
      "band_name": "2m",
      "motors_moving": false,
      "mode_name": "normal",
      "offline": false,
      "updated_at": "2026-05-19T12:34:56Z"
    }
    ```

### 2. Set Frequency & Mode
- **URL:** `/api/frequency`
- **Method:** `POST`
- **Description:** Sets the frequency and mode.
- **Request:**
  - Content-Type: `application/x-www-form-urlencoded`
  - Body:
    - `frequency` (integer, required): Frequency in kHz (1-65535)
    - `mode` (string, required): One of `normal`, `180`, `bidir`
- **Response:**
  - `200 OK` with `{ "status": "ok" }` on success
  - `400 Bad Request` on invalid input

### 3. Refresh State
- **URL:** `/api/refresh`
- **Method:** `POST`
- **Description:** Triggers a refresh of the controller state.
- **Response:**
  - `200 OK` with `{ "status": "ok" }` on success

### 4. Retract
- **URL:** `/api/retract`
- **Method:** `POST`
- **Description:** Retracts the antenna elements.
- **Response:**
  - `200 OK` with `{ "status": "ok" }` on success

### 5. Server-Sent Events (Live Updates)
- **URL:** `/api/events`
- **Method:** `GET`
- **Description:** Streams live status updates as [Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events).
- **Response:**
  - Content-Type: `text/event-stream`
  - Events of type `status` with JSON payload as above.

---

## Example Usage

### Get Status
```sh
curl http://localhost:8080/api/status
```

### Set Frequency
```sh
curl -X POST http://localhost:8080/api/frequency \
  -d "frequency=14500&mode=normal"
```

### Refresh State
```sh
curl -X POST http://localhost:8080/api/refresh
```

### Retract
```sh
curl -X POST http://localhost:8080/api/retract
```

---

## Notes
- All modifying endpoints use `POST` for simplicity.
- Frequency must be between 1 and 65535.
- Modes: `normal`, `180`, `bidir`.
- Live updates are available via SSE at `/api/events`.
