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
      "band_name": "20m",
      "motors_moving": false,
      "mode_name": "forward",
      "offline": false,
      "updated_at": "2026-05-19T12:34:56Z"
    }
    ```

### 2. Set Frequency
- **URL:** `/api/frequency`
- **Method:** `POST`
- **Description:** Sets the frequency. If `mode` is omitted, the current mode is retained.
- **Request:**
  - Content-Type: `application/x-www-form-urlencoded`
  - Body:
    - `frequency` (integer, required): Frequency in kHz (1-65535)
    - `mode` (string, optional): One of `forward`, `reverse`, `bidirectional`
- **Response:**
  - `200 OK` with `{ "status": "ok" }` on success
  - `400 Bad Request` on invalid input

### 3. Set Mode
- **URL:** `/api/mode`
- **Method:** `POST`
- **Description:** Sets the antenna mode without changing the current frequency.
- **Request:**
  - Content-Type: `application/x-www-form-urlencoded`
  - Body:
    - `mode` (string, required): One of `forward`, `reverse`, `bidirectional`
- **Response:**
  - `200 OK` with `{ "status": "ok" }` on success
  - `400 Bad Request` on invalid input

### 4. Refresh State
- **URL:** `/api/refresh`
- **Method:** `POST`
- **Description:** Triggers a refresh of the controller state.
- **Response:**
  - `200 OK` with `{ "status": "ok" }` on success

### 5. Retract
- **URL:** `/api/retract`
- **Method:** `POST`
- **Description:** Retracts the antenna elements.
- **Response:**
  - `200 OK` with `{ "status": "ok" }` on success

### 6. Server-Sent Events (Live Updates)
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
  -d "frequency=14500&mode=forward"
```

### Set Mode
```sh
curl -X POST http://localhost:8080/api/mode \
  -d "mode=bidirectional"
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
- Modes: `forward`, `reverse`, `bidirectional`.
- Live updates are available via SSE at `/api/events`.

---

## MQTT

The MQTT interface (station-model slot `<site>/<station>/ant-ctrl` plus Home Assistant
discovery) is documented in `ultrabeam-mqtt-api.md` — that file is the authoritative
on-the-wire contract. This document covers the web REST API only.
