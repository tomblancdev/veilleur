package web

import (
	"net/http"
	"strings"
)

// The published contract (power.md §5): anything in the lab can be a client
// of Le Veilleur with curl and this document — no library, no SDK.
func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(strings.ReplaceAll(openapiJSON, "{{VERSION}}", s.version)))
}

const openapiJSON = `{
  "openapi": "3.1.0",
  "info": {
    "title": "Le Veilleur",
    "version": "{{VERSION}}",
    "summary": "The watchman: claims decide which machines are awake.",
    "description": "A target is up while any claim on it - or on anything that requires it - is held. Guards only ever refuse to stop something. Every claim expires.",
    "license": { "name": "MIT" }
  },
  "servers": [{ "url": "/" }],
  "security": [{ "bearer": [] }, { "forwardAuth": [] }],
  "components": {
    "securitySchemes": {
      "bearer": { "type": "http", "scheme": "bearer", "description": "Machine clients: a token from the tokens file." },
      "forwardAuth": { "type": "apiKey", "in": "header", "name": "Remote-User", "description": "Set by the reverse proxy after SSO; believed only from a trusted proxy." }
    },
    "schemas": {
      "Claim": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "seq": { "type": "integer" },
          "subject": { "type": "string" },
          "via": { "type": "string" },
          "target": { "type": "string" },
          "reason": { "type": "string" },
          "held_since": { "type": "string", "format": "date-time" },
          "deadline": { "type": "string", "format": "date-time" },
          "release": { "type": "string", "enum": ["explicit", "idle", "deadline"] },
          "last_active": { "type": "string", "format": "date-time" },
          "released_at": { "type": "string", "format": "date-time" },
          "released_by": { "type": "string" }
        }
      },
      "ClaimRequest": {
        "type": "object",
        "properties": {
          "target": { "type": "string" },
          "reason": { "type": "string", "description": "Why. It ends up in Loki; write it for the person reading at 3 a.m." },
          "release": { "type": "string", "enum": ["explicit", "idle", "deadline"], "default": "explicit" },
          "hold": { "type": "string", "description": "Duration such as 2h. Clamped to the target's max_hold." },
          "idle_after": { "type": "string", "description": "Duration; required when release is idle and the target declares none." }
        }
      },
      "Target": {
        "type": "object",
        "properties": {
          "name": { "type": "string" },
          "kind": { "type": "string", "enum": ["node", "guest"] },
          "node": { "type": "string" },
          "vmid": { "type": "integer" },
          "requires": { "type": "array", "items": { "type": "string" } },
          "up": { "type": "boolean" },
          "wanted": { "type": "boolean" },
          "wanted_by": { "type": "array", "items": { "type": "string" } },
          "blocked": { "type": "string", "description": "Why it was not stopped: grace, dependent:<name> or guard:<name>." },
          "pending": { "type": "string" },
          "last_error": { "type": "string" }
        }
      },
      "Error": { "type": "object", "properties": { "error": { "type": "string" } } }
    }
  },
  "paths": {
    "/api/targets": {
      "get": {
        "summary": "The board: every target, its state, and what holds it up.",
        "responses": { "200": { "description": "the board", "content": { "application/json": { "schema": {
          "type": "object", "properties": {
            "at": { "type": "string", "format": "date-time" },
            "source": { "type": "string" },
            "observe_error": { "type": "string" },
            "targets": { "type": "array", "items": { "$ref": "#/components/schemas/Target" } } } } } } } }
      }
    },
    "/api/targets/{name}": {
      "get": {
        "summary": "One target.",
        "parameters": [{ "name": "name", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": {
          "200": { "description": "the target", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Target" } } } },
          "404": { "description": "no such target", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } }
        }
      }
    },
    "/api/targets/{name}/ensure": {
      "post": {
        "summary": "I need this up.",
        "description": "Takes a claim and drives the whole chain up (wake the node, then start the guest). Returns at once with what is still missing and a rough ETA; poll /api/targets/{name} for readiness.",
        "parameters": [{ "name": "name", "in": "path", "required": true, "schema": { "type": "string" } }],
        "requestBody": { "required": false, "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ClaimRequest" } } } },
        "responses": { "202": { "description": "claimed; the chain is coming up", "content": { "application/json": { "schema": {
          "type": "object", "properties": {
            "claim": { "$ref": "#/components/schemas/Claim" },
            "target": { "type": "string" },
            "up": { "type": "boolean" },
            "eta_seconds": { "type": "integer" },
            "chain": { "type": "array", "items": { "$ref": "#/components/schemas/Target" } } } } } } } }
      }
    },
    "/api/claims": {
      "get": { "summary": "Claims (yours, or all of them if you are an admin).", "responses": { "200": { "description": "claims" } } },
      "post": {
        "summary": "Take a claim without driving the chain up.",
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ClaimRequest" } } } },
        "responses": { "201": { "description": "the claim", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Claim" } } } } }
      }
    },
    "/api/claims/{id}": {
      "delete": {
        "summary": "Release a claim.",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "released" }, "403": { "description": "not your claim" }, "404": { "description": "no such claim" } }
      }
    },
    "/api/claims/{id}/heartbeat": {
      "post": {
        "summary": "Still in use.",
        "description": "Refreshes an idle-ruled claim. A reporter inside a guest calls this while somebody is actually using it.",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "the claim" } }
      }
    },
    "/api/fleet": { "get": { "summary": "The raw observation: nodes, guests, locks, quorum.", "responses": { "200": { "description": "snapshot" } } } },
    "/healthz": { "get": { "summary": "Liveness. 503 while the fleet cannot be observed.", "security": [], "responses": { "200": { "description": "ok" }, "503": { "description": "degraded" } } } },
    "/metrics": { "get": { "summary": "Prometheus text exposition.", "security": [], "responses": { "200": { "description": "metrics" } } } }
  }
}
`
