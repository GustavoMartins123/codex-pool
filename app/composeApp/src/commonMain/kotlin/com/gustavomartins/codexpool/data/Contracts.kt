package com.gustavomartins.codexpool.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement

val PoolJson = Json { ignoreUnknownKeys = true; encodeDefaults = true }

@Serializable
enum class Agent { @SerialName("codex") Codex, @SerialName("claude") Claude }

@Serializable
enum class WorkspaceMode {
    @SerialName("ephemeral") Ephemeral,
    @SerialName("path") Path,
    @SerialName("repo") Repo,
}

@Serializable
enum class PermissionMode { @SerialName("auto") Auto, @SerialName("gated") Gated }

@Serializable
enum class SessionState {
    @SerialName("created") Created,
    @SerialName("pending_runner") PendingRunner,
    @SerialName("claimed") Claimed,
    @SerialName("running") Running,
    @SerialName("waiting_input") WaitingInput,
    @SerialName("waiting_approval") WaitingApproval,
    @SerialName("orphaned") Orphaned,
    @SerialName("closing") Closing,
    @SerialName("closed") Closed,
}

@Serializable
data class Workspace(val mode: WorkspaceMode, val target: String? = null)

@Serializable
data class CreateSession(
    val agent: Agent,
    val model: String,
    val workspace: Workspace,
    @SerialName("permission_mode") val permissionMode: PermissionMode,
    val prompt: String? = null,
    @SerialName("reasoning_effort") val reasoningEffort: String = "",
) {
    fun validate() {
        require(model.isNotBlank()) { "Selecione um modelo do catálogo." }
        when (workspace.mode) {
            WorkspaceMode.Ephemeral -> require(workspace.target == null) { "Workspace efêmero não recebe destino." }
            WorkspaceMode.Path -> {
                require(!workspace.target.isNullOrBlank()) { "Informe o caminho permitido pelo runner." }
                require(permissionMode == PermissionMode.Gated) { "Workspace por caminho exige aprovação de comandos." }
            }
            WorkspaceMode.Repo -> require(!workspace.target.isNullOrBlank()) { "Informe a URL do repositório." }
        }
    }
}

@Serializable
data class Session(
    val id: String,
    val agent: Agent,
    val model: String,
    val workspace: Workspace,
    @SerialName("permission_mode") val permissionMode: PermissionMode,
    val state: SessionState,
    @SerialName("runner_id") val runnerId: String? = null,
    @SerialName("turn_id") val turnId: String? = null,
    @SerialName("closed_reason") val closedReason: String? = null,
    val approvals: List<Approval> = emptyList(),
    @SerialName("conversation_id") val conversationId: String,
    @SerialName("reasoning_effort") val reasoningEffort: String = "",
)

@Serializable data class TurnSettings(val model: String, @SerialName("reasoning_effort") val reasoningEffort: String = "")

@Serializable enum class ConversationStorage { @SerialName("active") Active, @SerialName("archived") Archived }
@Serializable data class MessagePreview(@SerialName("turn_id") val turnId: String, val role: String, val text: String)
@Serializable data class Conversation(
    val id: String, @SerialName("session_id") val sessionId: String, val agent: Agent, val model: String,
    val title: String, val storage: ConversationStorage, @SerialName("session_state") val sessionState: SessionState,
    @SerialName("last_interaction_at") val lastInteractionAt: String?, val preview: List<MessagePreview>,
    @SerialName("resume_available") val resumeAvailable: Boolean,
    val deleting: Boolean, @SerialName("delete_error") val deleteError: String? = null,
)
@Serializable data class ConversationPage(val conversations: List<Conversation>, @SerialName("next_cursor") val nextCursor: String? = null)
@Serializable data class LoginRequest(val email: String, val password: String)
@Serializable data class LoginResponse(val token: String, @SerialName("client_id") val clientId: String)
@Serializable data class Credentials(val baseUrl: String, val token: String, val clientId: String)
@Serializable data class TurnInput(val input: String, val model: String? = null,
    @SerialName("reasoning_effort") val reasoningEffort: String? = null)
// Separate signed bodies keep unsigned requests byte-identical for pools that predate signing.
@Serializable data class SignedTurnInput(val input: String, val model: String? = null,
    @SerialName("reasoning_effort") val reasoningEffort: String? = null,
    @SerialName("issued_at") val issuedAt: Long, val signatures: Map<String, String>)
@Serializable data class TurnMessage(@SerialName("turn_id") val turnId: String, val input: String)
@Serializable data class HistoryEvent(val id: Long, val type: String, val data: JsonElement)
@Serializable data class HistoryPage(
    val conversation: Conversation, val session: Session, val events: List<HistoryEvent>, val before: Long, val through: Long,
    @SerialName("has_older") val hasOlder: Boolean,
)
data class SequencedSessionEvent(val id: Long?, val event: SessionEvent)
@Serializable data class TurnStarted(@SerialName("turn_id") val turnId: String)
@Serializable enum class Decision { @SerialName("approve") Approve, @SerialName("deny") Deny }
@Serializable data class ApprovalDecision(val decision: Decision)
@Serializable data class SignedApprovalDecision(val decision: Decision, @SerialName("issued_at") val issuedAt: Long,
    val signatures: Map<String, String>)

@Serializable
data class Runner(
    val id: String,
    val name: String,
    val online: Boolean,
    val agents: List<Agent>,
    val revoked: Boolean = false,
    val labels: List<String> = emptyList(),
    @SerialName("allowed_paths") val allowedPaths: List<String> = emptyList(),
)
@Serializable data class RunnerList(val runners: List<Runner>)
@Serializable
data class CatalogModel(val id: String, val name: String? = null, val protocol: String,
    @SerialName("available_now") val availableNow: Boolean,
    val provider: String = "", val protocols: List<String> = emptyList(),
    @SerialName("reasoning_efforts") val reasoningEfforts: List<String> = emptyList(),
    @SerialName("default_reasoning_effort") val defaultReasoningEffort: String = "",
) {
    fun supports(agent: Agent): Boolean = (protocols + protocol).any {
        when (agent) { Agent.Codex -> it in listOf("openai", "responses"); Agent.Claude -> it == "anthropic" }
    }
    fun efforts(agent: Agent): List<String> = reasoningEfforts.filter {
        it in when (agent) {
            Agent.Codex -> listOf("none", "minimal", "low", "medium", "high", "xhigh", "max")
            Agent.Claude -> listOf("low", "medium", "high", "xhigh", "max")
        }
    }
}
@Serializable data class Catalog(val models: List<CatalogModel>)
@Serializable data class QuotaWindow(@SerialName("remaining_percent") val remainingPercent: Double,
    @SerialName("reported_accounts") val reportedAccounts: Int, @SerialName("updated_at") val updatedAt: String)
@Serializable data class ModelQuota(val model: String, val name: String, val provider: String,
    @SerialName("available_now") val availableNow: Boolean, val primary: QuotaWindow?, val secondary: QuotaWindow?)
@Serializable data class Quotas(val models: List<ModelQuota>, @SerialName("generated_at") val generatedAt: String)

@Serializable data class StateEvent(val state: SessionState)
@Serializable enum class DeltaKind {
    @SerialName("text") Text, @SerialName("thinking") Thinking,
    @SerialName("tool_call") ToolCall, @SerialName("tool_result") ToolResult,
}
@Serializable data class TurnDelta(@SerialName("turn_id") val turnId: String, val kind: DeltaKind, val content: String)
@Serializable data class TurnFinished(@SerialName("turn_id") val turnId: String, val tokens: Long,
    @SerialName("duration_ms") val durationMs: Long, @SerialName("exit_code") val exitCode: Int = 0)
@Serializable data class Approval(@SerialName("approval_id") val approvalId: String, val command: String, val context: String)
@Serializable data class ApprovalResolved(@SerialName("approval_id") val approvalId: String, val decision: Decision)
@Serializable enum class RunnerStatus { @SerialName("online") Online, @SerialName("offline") Offline, @SerialName("reconnected") Reconnected }
@Serializable data class RunnerStatusEvent(val status: RunnerStatus)
@Serializable data class ClosedEvent(val reason: String)
@Serializable data class ExecutionError(val message: String)

sealed interface SessionEvent {
    data class Settings(val value: TurnSettings) : SessionEvent
    data class Input(val value: TurnMessage) : SessionEvent
    data class State(val value: StateEvent) : SessionEvent
    data class Started(val value: TurnStarted) : SessionEvent
    data class Delta(val value: TurnDelta) : SessionEvent
    data class Finished(val value: TurnFinished) : SessionEvent
    data class Requested(val value: Approval) : SessionEvent
    data class Resolved(val value: ApprovalResolved) : SessionEvent
    data class RunnerChanged(val value: RunnerStatusEvent) : SessionEvent
    data class Closed(val value: ClosedEvent) : SessionEvent
    data class Error(val value: ExecutionError) : SessionEvent
}

fun decodeEvent(type: String, data: String): SessionEvent = when (type) {
    "session.settings" -> SessionEvent.Settings(PoolJson.decodeFromString(data))
    "turn.input" -> SessionEvent.Input(PoolJson.decodeFromString(data))
    "state" -> SessionEvent.State(PoolJson.decodeFromString(data))
    "turn.started" -> SessionEvent.Started(PoolJson.decodeFromString(data))
    "turn.delta" -> SessionEvent.Delta(PoolJson.decodeFromString(data))
    "turn.finished" -> SessionEvent.Finished(PoolJson.decodeFromString(data))
    "approval.requested" -> SessionEvent.Requested(PoolJson.decodeFromString(data))
    "approval.resolved" -> SessionEvent.Resolved(PoolJson.decodeFromString(data))
    "runner.status" -> SessionEvent.RunnerChanged(PoolJson.decodeFromString(data))
    "session.closed" -> SessionEvent.Closed(PoolJson.decodeFromString(data))
    "error" -> SessionEvent.Error(PoolJson.decodeFromString(data))
    else -> error("Evento de sessão desconhecido: $type")
}
