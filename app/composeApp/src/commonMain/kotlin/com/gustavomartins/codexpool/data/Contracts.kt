package com.gustavomartins.codexpool.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement

val PoolJson = Json { ignoreUnknownKeys = true; encodeDefaults = true }

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
@Serializable data class HistoryEvent(val id: Long, val type: String, val data: JsonElement)
@Serializable data class HistoryPage(
    val conversation: Conversation, val session: Session, val events: List<HistoryEvent>, val before: Long, val through: Long,
    @SerialName("has_older") val hasOlder: Boolean,
)
data class SequencedSessionEvent(val id: Long?, val event: SessionEvent)
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
