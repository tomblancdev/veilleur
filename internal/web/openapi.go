package web

import (
	"net/http"
	"strings"
)

// The published contract: anything in the lab can be a client with curl and
// this document — no SDK, no library. Three nouns: targets, signals, holds.
func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(strings.ReplaceAll(openapiJSON, "{{VERSION}}", s.version)))
}

const openapiJSON = `{
  "openapi": "3.1.0",
  "info": {
    "title": "Le Veilleur",
    "version": "{{VERSION}}",
    "summary": "The watchman: machines are awake while something says they are in use.",
    "description": "Ask for a target and everything it needs is raised, parents first. What keeps a thing up afterwards is a SIGNAL - a named question answered by a command on a node - not a lease anyone has to remember to renew. A signal that cannot be answered blocks a stop; it never permits one. Only a target named in a 'manages' list is ever stopped.",
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
      "Target": {
        "type": "object",
        "properties": {
          "name": { "type": "string" },
          "kind": { "type": "string", "enum": ["node", "guest"] },
          "node": { "type": "string", "description": "The machine that answers for it." },
          "needs": { "type": "array", "items": { "type": "string" }, "description": "The WAKE chain, and only the wake chain." },
          "min_uptime": { "type": "string", "description": "Not eligible to stop until it has been up this long AND every stop_when signal has answered at least once." },
          "up": { "type": "boolean" },
          "known": { "type": "boolean", "description": "false = its state probe could not be answered." },
          "managed": { "type": "boolean", "description": "false = never stopped by Le Veilleur, whatever the signals say." },
          "stop_when": { "type": "array", "items": { "type": "string" } },
          "blocked": { "type": "string", "description": "Why it is not being stopped: grace, min_uptime, hands-off, unknown:<signal>, held-by:<ref>." },
          "holds": { "type": "array", "items": { "$ref": "#/components/schemas/Hold" } }
        }
      },
      "Hold": {
        "type": "object",
        "description": "The only state a person writes. It has no expiry - a person decided, and a person can be asked - so it carries who and why, and ages loudly.",
        "properties": {
          "id": { "type": "string" },
          "target": { "type": "string" },
          "by": { "type": "string" },
          "reason": { "type": "string" },
          "since": { "type": "string", "format": "date-time" },
          "hands_off": { "type": "boolean", "description": "Also refuse to START it - for when you are working on the machine." }
        }
      },
      "Error": { "type": "object", "properties": { "error": { "type": "string" } } }
    }
  },
  "paths": {
    "/api/targets": {
      "get": {
        "summary": "The board: every target, its state, and why it is not stopping.",
        "responses": { "200": { "description": "the board" } }
      }
    },
    "/api/targets/{name}": {
      "get": {
        "summary": "One target.",
        "parameters": [{ "name": "name", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": {
          "200": { "description": "the target", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Target" } } } },
          "404": { "description": "no such target" }
        }
      }
    },
    "/api/targets/{name}/wake": {
      "post": {
        "summary": "I need this up.",
        "description": "Raises the target and everything it needs, parents first, skipping whatever is already up. The request is NOT remembered: once the chain is up, what keeps it up is whatever signal says it is in use. Refused if a hands-off hold stands.",
        "parameters": [{ "name": "name", "in": "path", "required": true, "schema": { "type": "string" } }],
        "requestBody": { "required": false, "content": { "application/json": { "schema": { "type": "object", "properties": {
          "reason": { "type": "string", "description": "Ends up in Loki; write it for the person reading at 3 a.m." },
          "wait": { "type": "boolean", "description": "Block until the chain is up (default: return at once)." } } } } } },
        "responses": {
          "200": { "description": "the chain is up (wait: true)" },
          "202": { "description": "the chain is being raised" },
          "404": { "description": "no such target" },
          "409": { "description": "refused - hands-off, or something would not come up" }
        }
      }
    },
    "/api/signals": {
      "get": {
        "summary": "Every signal, its last answer, and what it means.",
        "description": "true / false / unknown. Unknown blocks a stop; it never permits one.",
        "responses": { "200": { "description": "signals" } }
      }
    },
    "/api/holds": {
      "get": { "summary": "Every hold a person has placed.", "responses": { "200": { "description": "holds" } } },
      "post": {
        "summary": "Keep something up until further notice.",
        "description": "Admins only, and a reason is required: a hold has no expiry and may outlive the memory of why it was taken.",
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type": "object",
          "required": ["target", "reason"],
          "properties": {
            "target": { "type": "string" },
            "reason": { "type": "string" },
            "hands_off": { "type": "boolean" } } } } } },
        "responses": { "201": { "description": "the hold", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Hold" } } } },
                       "400": { "description": "no target, or no reason" },
                       "403": { "description": "admins only" } }
      }
    },
    "/api/holds/{id}": {
      "delete": {
        "summary": "Lift a hold.",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "lifted" }, "403": { "description": "admins only" }, "404": { "description": "no such hold" } }
      }
    },
    "/healthz": { "get": { "summary": "Liveness. 503 while nothing in the fleet can be observed.", "security": [], "responses": { "200": { "description": "ok" }, "503": { "description": "degraded" } } } },
    "/metrics": { "get": { "summary": "Prometheus text exposition.", "security": [], "responses": { "200": { "description": "metrics" } } } }
  }
}
`
