package com.gustavomartins.codexpool.ui

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.gustavomartins.codexpool.data.*
import io.ktor.client.HttpClient
import io.ktor.client.plugins.HttpRequestTimeoutException
import kotlinx.io.IOException
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.*
import kotlinx.serialization.SerializationException

fun Throwable.userMessage(): String = when (this) {
    is PoolApiException -> message.orEmpty()
    is SessionProtocolException -> message.orEmpty()
    is SerializationException -> "Resposta incompatível com o contrato do pool."
    is IllegalArgumentException -> message.orEmpty()
    else -> "A operação falhou (${this::class.simpleName}). Verifique a conexão e tente novamente."
}

data class AuthState(val loading: Boolean = true, val credentials: Credentials? = null, val error: String? = null)

class AuthViewModel(private val store: CredentialStore, private val client: HttpClient, private val poolUrl: String) : ViewModel() {
    private val mutable = MutableStateFlow(AuthState())
    val state = mutable.asStateFlow()
    init {
        viewModelScope.launch {
            try {
                val credentials = store.read()
                if (credentials != null) {
                    require(credentials.baseUrl == poolUrl) { "A credencial salva pertence a outro pool. Limpe a credencial para entrar novamente." }
                    PoolApi(client, credentials)
                }
                mutable.value = AuthState(loading = false, credentials = credentials)
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.value = AuthState(loading = false, error = e.userMessage()) }
        }
    }

    fun login(username: String, password: String) {
        if (mutable.value.loading) return
        mutable.update { it.copy(loading = true, error = null) }
        viewModelScope.launch {
            try {
                val credentials = PoolApi.login(client, poolUrl, username, password)
                store.write(credentials)
                mutable.value = AuthState(loading = false, credentials = credentials)
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.update { it.copy(loading = false, error = e.userMessage()) } }
        }
    }

    fun logout() {
        if (mutable.value.loading) return
        mutable.update { it.copy(loading = true, error = null) }
        viewModelScope.launch {
            try {
                mutable.value.credentials?.let { credentials ->
                    try { PoolApi(client, credentials).logout() }
                    catch (e: PoolApiException) { if (e.status != 401) throw e }
                }
                store.clear()
                mutable.value = AuthState(loading = false)
            }
            catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.update { it.copy(loading = false, error = e.userMessage()) } }
        }
    }
}

enum class DashboardTab { Sessions, Runners, Quotas }
data class DashboardState(
    val tab: DashboardTab = DashboardTab.Sessions,
    val loading: Boolean = false,
    val sessions: List<Session>? = null,
    val runners: List<Runner>? = null,
    val quotas: Quotas? = null,
    val catalog: List<CatalogModel>? = null,
    val pairingOpen: Boolean = false,
    val pairing: PairingReview? = null,
    val pairingComplete: Boolean = false,
    val error: String? = null,
)

class DashboardViewModel(private val api: PoolApi) : ViewModel() {
    private val mutable = MutableStateFlow(DashboardState())
    val state = mutable.asStateFlow()
    init { refresh() }

    fun select(tab: DashboardTab) {
        if (mutable.value.loading) return
        mutable.update { it.copy(tab = tab, error = null) }
        refresh()
    }

    fun refresh() = action {
        when (mutable.value.tab) {
            DashboardTab.Sessions -> {
                mutable.update { it.copy(sessions = null) }
                val sessions = api.sessions()
                mutable.update { it.copy(sessions = sessions) }
            }
            DashboardTab.Runners -> {
                mutable.update { it.copy(runners = null) }
                val runners = api.runners()
                mutable.update { it.copy(runners = runners) }
            }
            DashboardTab.Quotas -> {
                mutable.update { it.copy(quotas = null) }
                val quotas = api.quotas()
                mutable.update { it.copy(quotas = quotas) }
            }
        }
    }

    fun loadCatalog() = action {
        mutable.update { it.copy(catalog = null) }
        val models = api.catalog()
        mutable.update { it.copy(catalog = models) }
    }

    fun create(request: CreateSession, onCreated: (String) -> Unit) = action {
        request.validate()
        require(mutable.value.catalog?.any { it.id == request.model && it.availableNow } == true) { "Selecione um modelo disponível no catálogo." }
        val session = api.create(request)
        onCreated(session.id)
    }

    fun openPairing() { mutable.update { it.copy(pairingOpen = true, pairing = null, pairingComplete = false, error = null) } }
    fun dismissPairing() { if (!mutable.value.loading) mutable.update { it.copy(pairingOpen = false, pairing = null, pairingComplete = false, error = null) } }
    fun reviewPairing(link: String) = action {
        mutable.update { it.copy(pairingOpen = true, pairing = null, pairingComplete = false) }
        val pairing = api.reviewPairing(link)
        mutable.update { it.copy(pairing = pairing) }
    }
    fun decidePairing(id: String, decision: Decision, code: String) = action {
        require(mutable.value.pairing?.id == id) { "O pedido de pareamento mudou. Leia o QR novamente." }
        api.decidePairing(id, decision, code)
        mutable.update { it.copy(pairing = null, pairingComplete = true) }
    }
    fun revoke(id: String) = action {
        api.revoke(id)
        val runners = api.runners()
        mutable.update { it.copy(runners = runners) }
    }

    private fun action(block: suspend () -> Unit) {
        if (mutable.value.loading) return
        mutable.update { it.copy(loading = true, error = null) }
        viewModelScope.launch {
            try { block() }
            catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.update { it.copy(error = e.userMessage()) } }
            finally { mutable.update { it.copy(loading = false) } }
        }
    }
}

enum class StreamConnection { Disconnected, Connecting, Live }
data class TranscriptEntry(val turnId: String, val kind: String, val content: String)
data class SessionScreenState(
    val session: Session? = null,
    val connection: StreamConnection = StreamConnection.Disconnected,
    val entries: List<TranscriptEntry> = emptyList(),
    val approvals: List<Approval> = emptyList(),
    val runnerStatus: RunnerStatus? = null,
    val busy: Boolean = false,
    val error: String? = null,
    val eventRevision: Long = 0,
) {
    val canSend get() = session?.state == SessionState.WaitingInput && connection == StreamConnection.Live && !busy
    val canCancel get() = session?.state in listOf(SessionState.Running, SessionState.WaitingApproval) && connection == StreamConnection.Live && !busy
}

fun reduceEvent(state: SessionScreenState, event: SessionEvent): SessionScreenState = when (event) {
    is SessionEvent.State -> state.copy(session = state.session?.copy(state = event.value.state))
    is SessionEvent.Started -> state.copy(session = state.session?.copy(state = SessionState.Running, turnId = event.value.turnId))
    is SessionEvent.Delta -> {
        val delta = event.value
        val kind = delta.kind.name
        val last = state.entries.lastOrNull()
        val entries = if (last?.turnId == delta.turnId && last.kind == kind) {
            state.entries.dropLast(1) + last.copy(content = last.content + delta.content)
        } else state.entries + TranscriptEntry(delta.turnId, kind, delta.content)
        state.copy(entries = entries)
    }
    is SessionEvent.Finished -> state.copy(
        session = state.session?.copy(state = SessionState.WaitingInput, turnId = null),
        entries = state.entries + TranscriptEntry(event.value.turnId, "summary",
            "${event.value.tokens} tokens · ${event.value.durationMs} ms" + when (event.value.exitCode) {
                0 -> ""
                130 -> " · cancelado"
                else -> " · falha (código ${event.value.exitCode})"
            }),
    )
    is SessionEvent.Requested -> state.copy(
        session = state.session?.copy(state = SessionState.WaitingApproval),
        approvals = state.approvals.filterNot { it.approvalId == event.value.approvalId } + event.value,
    )
    is SessionEvent.Resolved -> state.copy(approvals = state.approvals.filterNot { it.approvalId == event.value.approvalId })
    is SessionEvent.RunnerChanged -> state.copy(runnerStatus = event.value.status)
    is SessionEvent.Closed -> state.copy(session = state.session?.copy(state = SessionState.Closed, closedReason = event.value.reason), approvals = emptyList())
    is SessionEvent.Error -> state.copy(error = event.value.message)
}

class SessionViewModel(private val api: PoolApi, private val id: String) : ViewModel() {
    private val mutable = MutableStateFlow(SessionScreenState())
    val state = mutable.asStateFlow()
    private var stream: Job? = null
    private var streamGeneration = 0L
    private var messageSequence = 0L
    private var lastEventId: String? = null

    fun connect() {
        if (stream?.isActive == true) return
        val generation = ++streamGeneration
        mutable.update { it.copy(connection = StreamConnection.Connecting, error = null) }
        stream = viewModelScope.launch {
            var reconnectDelay = 500L
            try {
                while (isActive && generation == streamGeneration) {
                    try {
                        val session = api.session(id)
                        if (generation != streamGeneration) return@launch
                        mutable.update { it.copy(session = session, approvals = session.approvals) }
                        // Replay the last deltas even if the session closed while offline.
                        if (session.state == SessionState.Closed && lastEventId == null) break
                        api.events(id, lastEventId, onCursor = { cursor ->
                            if (generation == streamGeneration) lastEventId = cursor
                        }) {
                            if (generation == streamGeneration) mutable.update {
                                it.copy(connection = StreamConnection.Live, error = null)
                            }
                        }.collect { event ->
                            if (generation == streamGeneration) {
                                mutable.update { reduceEvent(it, event).copy(eventRevision = it.eventRevision + 1) }
                                reconnectDelay = 500L
                                if (event is SessionEvent.Error) throw SessionProtocolException(event.value.message)
                            }
                        }
                        if (mutable.value.session?.state == SessionState.Closed) break
                    } catch (e: CancellationException) { throw e }
                    catch (e: Exception) {
                        if (generation != streamGeneration) return@launch
                        val retryable = e is IOException || e is HttpRequestTimeoutException ||
                            (e is PoolApiException && e.status in listOf(408, 429, 502, 503, 504))
                        if (!retryable) {
                            mutable.update { it.copy(error = e.userMessage()) }
                            break
                        }
                    }
                    if (!isActive || generation != streamGeneration || mutable.value.session?.state == SessionState.Closed) break
                    mutable.update { it.copy(connection = StreamConnection.Connecting, error = "SSE desconectado; reconectando...") }
                    delay(reconnectDelay)
                    reconnectDelay = (reconnectDelay * 2).coerceAtMost(5_000L)
                }
            } finally {
                if (generation == streamGeneration) mutable.update { it.copy(connection = StreamConnection.Disconnected) }
            }
        }
    }

    fun disconnect() {
        streamGeneration++
        stream?.cancel()
        stream = null
        mutable.update { it.copy(connection = StreamConnection.Disconnected) }
    }

    fun send(input: String, onSent: () -> Unit) {
        if (!mutable.value.canSend) return
        val localTurnId = "local-${++messageSequence}"
        action {
            val revision = mutable.value.eventRevision
            mutable.update { it.copy(entries = it.entries + TranscriptEntry(localTurnId, "user", input)) }
            val turn = try { api.turn(id, input) }
            catch (e: Exception) {
                mutable.update { it.copy(entries = it.entries.filterNot { entry -> entry.turnId == localTurnId }) }
                throw e
            }
            mutable.update {
                it.copy(session = if (it.eventRevision == revision) it.session?.copy(state = SessionState.Running, turnId = turn.turnId) else it.session,
                    entries = it.entries.map { entry -> if (entry.turnId == localTurnId) entry.copy(turnId = turn.turnId) else entry })
            }
            onSent()
        }
    }
    fun cancel() = action { api.cancel(id) }
    fun close() = action {
        val revision = mutable.value.eventRevision
        api.close(id)
        mutable.update { if (it.eventRevision == revision) it.copy(session = it.session?.copy(state = SessionState.Closing)) else it }
    }
    fun decide(approvalId: String, decision: Decision) = action {
        require(mutable.value.approvals.any { it.approvalId == approvalId }) { "A aprovação não está pendente." }
        api.decide(id, approvalId, decision)
        // Only the authoritative SSE event removes the pending approval.
    }

    private fun action(block: suspend () -> Unit) {
        if (mutable.value.busy || mutable.value.connection != StreamConnection.Live) return
        mutable.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            try {
                block()
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.update { it.copy(error = e.userMessage()) } }
            finally { mutable.update { it.copy(busy = false) } }
        }
    }
}
