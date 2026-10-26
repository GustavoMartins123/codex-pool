package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import kotlinx.serialization.json.jsonObject
import kotlin.test.*

class GeneratedRunnerContractTest {
    @Test fun sessionRequestsKeepTheirCanonicalWireNamesAndDefaults() {
        val draft = CreateSession(Agent.Codex, "synthetic", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated, "first input")
        val json = PoolJson.parseToJsonElement(PoolJson.encodeToString(draft)).jsonObject
        assertEquals(setOf("agent", "model", "workspace", "permission_mode", "initial_input", "reasoning_effort"), json.keys)
        assertFalse("prompt" in json)
        assertEquals(draft, PoolJson.decodeFromString<CreateSession>(json.toString()))
        val turn = PoolJson.decodeFromString<TurnInput>("""{"input":"continue"}""")
        assertEquals(0L, turn.issuedAt)
        assertNull(turn.pendingInputId)
        assertTrue(turn.signatures.isEmpty())
        assertFails { PoolJson.decodeFromString<CreateSession>("""{"model":"missing required fields"}""") }
        assertFails { PoolJson.decodeFromString<Workspace>("""{"mode":"unknown-mode"}""") }
    }

    @Test fun serverSessionProjectionPreservesPendingDraftAndUnknownServerMetadata() {
        val session = PoolJson.decodeFromString<Session>("""{
            "id":"s","conversation_id":"c","principal_id":"owner","client_id":"app",
            "agent":"claude","model":"synthetic","workspace":{"mode":"ephemeral"},
            "permission_mode":"gated","state":"waiting_input","runner_id":"r",
            "pending_input":{"id":"draft","input":"first input"},"created_at":"2026-01-01T00:00:00Z"
        }""")
        assertEquals("c", session.conversationId)
        assertEquals(PendingInput("draft", "first input"), session.pendingInput)
        assertEquals(SessionState.WaitingInput, session.state)
        assertTrue(session.approvals.isEmpty())
        assertEquals("", session.reasoningEffort)
    }

    @Test fun eventDecoderRetainsDistinctThinkingToolsAndAgentResponses() {
        for (kind in DeltaKind.entries) {
            val event = decodeEvent("turn.delta", """{"turn_id":"t","kind":"${kind.wire}","content":"event content"}""")
            assertEquals(kind, assertIs<SessionEvent.Delta>(event).value.kind)
        }
        val finished = assertIs<SessionEvent.Finished>(decodeEvent("turn.finished", """{"turn_id":"t","tokens":42,"duration_ms":10}"""))
        assertEquals(42L, finished.value.tokens)
        assertEquals(0, finished.value.exitCode)
        assertIs<SessionEvent.Closed>(decodeEvent("session.closed", """{"reason":"auto_permissions_disabled"}"""))
        assertFails { decodeEvent("unknown.event", "{}") }
        assertFails { decodeEvent("turn.delta", """{"turn_id":"t","kind":"unknown","content":"x"}""") }
    }
}
