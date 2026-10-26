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
    is BootstrapFailure -> message.orEmpty()
    else -> "A operação falhou (${this::class.simpleName}). Verifique a conexão e tente novamente."
}

data class AuthState(val loading: Boolean = true, val accounts: PoolAccounts? = null, val error: String? = null) {
    val credentials get() = accounts?.selected?.credentials
}

class AuthViewModel(private val store: CredentialStore, private val client: HttpClient, private val poolUrl: String) : ViewModel() {
    private val mutable = MutableStateFlow(AuthState())
    val state = mutable.asStateFlow()
    init {
        viewModelScope.launch {
            try {
                var accounts = store.read()?.validate() ?: PoolAccounts.initial(poolUrl)
                val defaultUrl = normalizePoolUrl(poolUrl)
                if (accounts.providers.none { it.baseUrl == defaultUrl }) {
                    accounts = accounts.copy(providers = accounts.providers + PoolProvider("Padrão", defaultUrl)).validate()
                }
                store.write(accounts)
                mutable.value = AuthState(loading = false, accounts = accounts)
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.value = AuthState(loading = false, error = e.userMessage()) }
        }
    }

    fun login(username: String, password: String) = action { accounts ->
        val credentials = PoolApi.login(client, accounts.selectedUrl, username, password)
        accounts.withCredentials(credentials)
    }

    fun selectProvider(url: String) = action { accounts ->
        require(accounts.providers.any { it.baseUrl == url }) { "Provedor não cadastrado." }
        accounts.copy(selectedUrl = url).validate()
    }

    fun addProvider(name: String, url: String) = action { accounts ->
        val base = normalizePoolUrl(url)
        val label = name.trim()
        require(label.isNotBlank() && label.length <= 60) { "Informe um nome com até 60 caracteres." }
        require(accounts.providers.none { it.baseUrl == base }) { "Esse domínio já está cadastrado." }
        require(accounts.providers.none { it.name.equals(label, ignoreCase = true) }) { "Esse nome já está em uso." }
        accounts.copy(selectedUrl = base, providers = accounts.providers + PoolProvider(label, base)).validate()
    }

    fun logout() = action { accounts ->
        accounts.selected.credentials?.let { credentials ->
            try { PoolApi(client, credentials).logout() }
            catch (e: PoolApiException) { if (e.status != 401) throw e }
        }
        accounts.withCredentials(null)
    }

    /** Saves a runner command key for the selected pool; pairing must not proceed if this fails. */
    suspend fun addCommandKey(key: String, owner: Credentials? = null) {
        val accounts = requireNotNull(mutable.value.accounts) { "Armazenamento de provedores indisponível." }
        val updated = accounts.withCommandKey(key, owner)
        store.write(updated)
        mutable.update { it.copy(accounts = updated) }
    }

    fun resetStorage() {
        if (mutable.value.loading || mutable.value.accounts != null) return
        mutable.update { it.copy(loading = true, error = null) }
        viewModelScope.launch {
            try {
                store.clear()
                val accounts = PoolAccounts.initial(poolUrl)
                store.write(accounts)
                mutable.value = AuthState(loading = false, accounts = accounts)
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.update { it.copy(loading = false, error = e.userMessage()) } }
        }
    }

    private fun action(block: suspend (PoolAccounts) -> PoolAccounts) {
        if (mutable.value.loading || mutable.value.accounts == null) return
        val accounts = requireNotNull(mutable.value.accounts)
        mutable.update { it.copy(loading = true, error = null) }
        viewModelScope.launch {
            try {
                val updated = block(accounts).validate()
                store.write(updated)
                mutable.value = AuthState(loading = false, accounts = updated)
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.update { it.copy(loading = false, error = e.userMessage()) } }
        }
    }
}

enum class DashboardTab { Sessions, Runners, Quotas }
data class DashboardState(
    val tab: DashboardTab = DashboardTab.Sessions,
    val loading: Boolean = false,
    val conversations: List<Conversation>? = null,
    val storage: ConversationStorage = ConversationStorage.Active,
    val nextCursor: String? = null,
    val runners: List<Runner>? = null,
    val quotas: Quotas? = null,
    val catalog: List<CatalogModel>? = null,
    val pairingOpen: Boolean = false,
    val pairing: PairingReview? = null,
    val pairingComplete: Boolean = false,
    val bootstrap: BootstrapProgress? = null,
    val error: String? = null,
)

class DashboardViewModel(
    private val api: PoolApi,
    private val signer: CommandSigner = CommandSigner.Disabled,
    private val bootstrapClients: BootstrapClientFactory? = null,
) : ViewModel() {
    private val mutable = MutableStateFlow(DashboardState())
    val state = mutable.asStateFlow()
    private var pairingKey: String? = null
    private var bootstrap: BootstrapCoordinator? = null
    private val bootstrapJobs = mutableListOf<Job>()
    init { refresh() }

    fun select(tab: DashboardTab) {
        if (mutable.value.loading) return
        mutable.update { it.copy(tab = tab, error = null) }
        refresh()
    }

    fun refresh() = action {
        when (mutable.value.tab) {
            DashboardTab.Sessions -> {
                mutable.update { it.copy(conversations = null, nextCursor = null) }
                val page = api.conversations(mutable.value.storage)
                mutable.update { it.copy(conversations = page.conversations, nextCursor = page.nextCursor) }
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

    fun selectStorage(storage: ConversationStorage) {
        if (mutable.value.loading || mutable.value.storage == storage) return
        mutable.update { it.copy(storage = storage) }
        refresh()
    }

    fun loadMoreConversations() = action {
        val cursor = requireNotNull(mutable.value.nextCursor)
        val page = api.conversations(mutable.value.storage, cursor)
        require(page.nextCursor != cursor) { "O cursor da lista não avançou." }
        val combined = mutable.value.conversations.orEmpty() + page.conversations
        require(combined.map { it.id }.distinct().size == combined.size) { "A lista retornou conversas repetidas." }
        mutable.update { it.copy(conversations = combined, nextCursor = page.nextCursor) }
    }

    fun loadCatalog() = action {
        mutable.update { it.copy(catalog = null) }
        val models = api.catalog()
        val runners = api.runners()
        mutable.update { it.copy(catalog = models.forRunners(runners.filter { signer.canSignFor(it.commandKeyId) }), runners = runners) }
    }

    fun create(request: CreateSession, onCreated: (String) -> Unit) = action {
        request.validate()
        require(mutable.value.catalog?.any { it.id == request.model && it.availableNow && it.supports(request.agent) &&
            (request.reasoningEffort.isEmpty() || request.reasoningEffort in it.efforts(request.agent)) } == true) { "Selecione um modelo e esforço confirmados por um runner online." }
        // The pool persists the draft before acknowledging session creation.
        // Execution still waits for a signed turn bound to the returned session.
        check(signer.enabled) { CommandSigner.MISSING_KEY }
        val runner = mutable.value.runners.orEmpty().firstOrNull { runner ->
            runner.online && !runner.revoked && runner.modelsKnown && signer.canSignFor(runner.commandKeyId) && runner.models.any {
                it.agent == request.agent && it.model == request.model &&
                    (request.reasoningEffort.isEmpty() || request.reasoningEffort in it.reasoningEfforts)
            }
        }
        require(runner != null) { "Nenhum runner online confirmou essas configurações. Atualize os modelos." }
        val session = api.create(request.copy(runnerId = runner.id))
        onCreated(session.conversationId)
    }

    fun openPairing() { mutable.update { it.copy(pairingOpen = true, pairing = null, pairingComplete = false, error = null) } }
    fun dismissPairing() {
        if (mutable.value.loading) return
        pairingKey = null
        stopBootstrap()
        mutable.update { it.copy(pairingOpen = false, pairing = null, pairingComplete = false, bootstrap = null, error = null) }
    }
    fun reviewPairing(link: String) = action {
        mutable.update { it.copy(pairingOpen = true, pairing = null, pairingComplete = false, bootstrap = null) }
        pairingKey = null
        stopBootstrap()
        when (val qr = parseRunnerQr(link, api.credentials.baseUrl, kotlin.time.Clock.System.now())) {
            is RunnerQr.Bootstrap -> startBootstrap(qr.payload)
            is RunnerQr.Pairing -> {
                val pairing = api.reviewPairing(link)
                pairingKey = qr.qr.commandKey
                mutable.update { it.copy(pairing = pairing) }
            }
        }
    }

    private fun startBootstrap(payload: BootstrapPayload) {
        val factory = requireNotNull(bootstrapClients) {
            "Este aparelho ainda não pareia runners pela rede local. No computador, use runnerd pair --url com o endereço do pool."
        }
        // The coordinator keeps this screen's account: api and signer belong to it.
        val coordinator = BootstrapCoordinator(api, { BootstrapApi.connect(factory, payload) }, signer::remember)
        bootstrap = coordinator
        bootstrapJobs += viewModelScope.launch { coordinator.progress.collect { p -> mutable.update { it.copy(bootstrap = p) } } }
        runBootstrap { it.prepare() }
    }
    fun approveBootstrap() = runBootstrap { it.approve() }
    fun denyBootstrap() = runBootstrap { it.deny() }

    private fun runBootstrap(block: suspend (BootstrapCoordinator) -> Unit) {
        val coordinator = bootstrap ?: return
        // Failures land in the coordinator's progress, which the dialog shows.
        bootstrapJobs += viewModelScope.launch { runCatching { block(coordinator) }.exceptionOrNull()?.let { if (it is CancellationException) throw it } }
    }

    private fun stopBootstrap() {
        val coordinator = bootstrap ?: return
        bootstrap = null
        bootstrapJobs.forEach { it.cancel() }
        bootstrapJobs.clear()
        // The runner should hear the cancellation even if the screen closes meanwhile.
        viewModelScope.launch { withContext(NonCancellable) { coordinator.cancel() } }
    }

    override fun onCleared() { bootstrap?.close() }
    fun decidePairing(id: String, decision: Decision, code: String) = action {
        require(mutable.value.pairing?.id == id) { "O pedido de pareamento mudou. Leia o QR novamente." }
        // Keep the runner's key before approving: a runner whose key was lost refuses every command.
        val key = pairingKey
        if (decision == Decision.Approve && key != null) signer.remember(key)
        api.decidePairing(id, decision, code)
        pairingKey = null
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
data class TranscriptEntry(val turnId: String, val kind: String, val content: String, val keyEventId: Long)
data class SessionScreenState(
    val catalog: List<CatalogModel>? = null,
    val catalogLoading: Boolean = false,
    val catalogError: String? = null,
    val selectedSettings: TurnSettings? = null,
    val session: Session? = null,
    val conversation: Conversation? = null,
    val connection: StreamConnection = StreamConnection.Disconnected,
    val entries: List<TranscriptEntry> = emptyList(),
    val approvals: List<Approval> = emptyList(),
    val runnerStatus: RunnerStatus? = null,
    val busy: Boolean = false,
    val error: String? = null,
    val eventRevision: Long = 0,
    val historyBefore: Long? = null,
    val historyThrough: Long? = null,
    val hasOlder: Boolean = false,
    val loadingOlder: Boolean = false,
    val historyError: String? = null,
    val deleted: Boolean = false,
) {
    val turnSettings get() = selectedSettings ?: session?.let { TurnSettings(it.model, it.reasoningEffort) }
    val canSend get() = session?.state == SessionState.WaitingInput && connection == StreamConnection.Live && !busy
    val canCancel get() = session?.state in listOf(SessionState.Running, SessionState.WaitingApproval) && connection == StreamConnection.Live && !busy
}

fun reduceEvent(state: SessionScreenState, event: SessionEvent, eventId: Long?): SessionScreenState = when (event) {
    is SessionEvent.Settings -> state.copy(session = state.session?.copy(model = event.value.model, reasoningEffort = event.value.reasoningEffort))
    is SessionEvent.Input -> state.copy(entries = state.entries.filterNot { it.kind == "user" && it.turnId == event.value.turnId } +
        TranscriptEntry(event.value.turnId, "user", event.value.input, requireNotNull(eventId)))
    is SessionEvent.State -> state.copy(session = state.session?.copy(state = event.value.state))
    is SessionEvent.Started -> state.copy(session = state.session?.copy(state = SessionState.Running, turnId = event.value.turnId))
    is SessionEvent.Delta -> {
        val delta = event.value
        val kind = delta.kind.name
        val last = state.entries.lastOrNull()
        val entries = if (last?.turnId == delta.turnId && last.kind == kind) {
            state.entries.dropLast(1) + last.copy(content = last.content + delta.content)
        } else state.entries + TranscriptEntry(delta.turnId, kind, delta.content, requireNotNull(eventId))
        state.copy(entries = entries)
    }
    is SessionEvent.Finished -> state.copy(
        session = state.session?.copy(state = SessionState.WaitingInput, turnId = null),
        entries = state.entries + TranscriptEntry(event.value.turnId, "summary",
            "${event.value.tokens} tokens · ${event.value.durationMs} ms" + when (event.value.exitCode) {
                0 -> ""
                130 -> " · cancelado"
                else -> " · falha (código ${event.value.exitCode})"
            }, requireNotNull(eventId)),
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

class SessionViewModel(private val api: PoolApi, private val id: String, private val signer: CommandSigner = CommandSigner.Disabled) : ViewModel() {
    private val mutable = MutableStateFlow(SessionScreenState())
    val state = mutable.asStateFlow()
    private var stream: Job? = null
    private var streamGeneration = 0L
    private var lastEventId: String? = null
    private var older: Job? = null

    fun loadModels() {
        if (mutable.value.catalogLoading) return
        mutable.update { it.copy(catalogLoading = true, catalogError = null) }
        viewModelScope.launch {
            try {
                val catalog = api.catalog()
                val runnerId = mutable.value.session?.runnerId
                val runners = api.runners()
                mutable.update { it.copy(catalog = if (runnerId == null) emptyList() else catalog.forRunners(runners, runnerId)) }
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.update { it.copy(catalogError = e.userMessage()) } }
            finally { mutable.update { it.copy(catalogLoading = false) } }
        }
    }

    fun selectSettings(settings: TurnSettings) {
        val current = mutable.value
        if (current.busy) return
        val agent = current.session?.agent ?: return
        val model = current.catalog?.singleOrNull { it.id == settings.model && it.availableNow && it.supports(agent) } ?: return
        if (settings.reasoningEffort.isNotEmpty() && settings.reasoningEffort !in model.efforts(agent)) return
        mutable.update { it.copy(selectedSettings = settings) }
    }

    fun connect() {
        if (stream?.isActive == true) return
        val generation = ++streamGeneration
        mutable.update { it.copy(connection = StreamConnection.Connecting, error = null) }
        stream = viewModelScope.launch {
            var reconnectDelay = 500L
            try {
                while (isActive && generation == streamGeneration) {
                    try {
                        val loadingHistory = lastEventId == null
                        val session = if (loadingHistory) {
                            val history = api.history(id)
                            if (generation != streamGeneration) return@launch
                            val restored = history.events.fold(SessionScreenState(session = history.session, conversation = history.conversation)) { state, event ->
                                reduceEvent(state, decodeEvent(event.type, event.data.toString()), event.id)
                            }
                            lastEventId = history.through.toString()
                            mutable.update { restored.copy(session = history.session, approvals = history.session.approvals,
                                historyBefore = history.before, historyThrough = history.through, hasOlder = history.hasOlder,
                                connection = StreamConnection.Connecting, eventRevision = it.eventRevision + 1) }
                            history.session
                        } else api.session(requireNotNull(mutable.value.session).id)
                        if (generation != streamGeneration) return@launch
                        mutable.update { it.copy(session = session, approvals = session.approvals) }
                        // Replay the last deltas even if the session closed while offline.
                        if (session.state == SessionState.Closed && loadingHistory) break
                        api.events(id, lastEventId, onCursor = { cursor ->
                            if (generation == streamGeneration) lastEventId = cursor
                        }) {
                            if (generation == streamGeneration) {
                                mutable.update { it.copy(connection = StreamConnection.Live, error = null) }
                                sendPendingPrompt()
                            }
                        }.collect { frame ->
                            val event = frame.event
                            if (generation == streamGeneration) {
                                mutable.update { reduceEvent(it, event, frame.id).copy(eventRevision = it.eventRevision + 1) }
                                reconnectDelay = 500L
                                if (event is SessionEvent.Error) throw SessionProtocolException(event.value.message)
                                sendPendingPrompt()
                            }
                        }
                        if (mutable.value.session?.state == SessionState.Closed) {
                            val conversation = api.conversation(id)
                            if (generation == streamGeneration) mutable.update { it.copy(conversation = conversation) }
                            break
                        }
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
        older?.cancel()
        mutable.update { it.copy(connection = StreamConnection.Disconnected, loadingOlder = false) }
    }

    fun loadOlder() {
        if (!mutable.value.hasOlder || mutable.value.loadingOlder) return
        val generation = streamGeneration
        val before = requireNotNull(mutable.value.historyBefore)
        val through = requireNotNull(mutable.value.historyThrough)
        mutable.update { it.copy(loadingOlder = true, historyError = null) }
        older = viewModelScope.launch {
            try {
                val page = api.history(id, before, through)
                val historical = page.events.fold(SessionScreenState()) { state, event ->
                    reduceEvent(state, decodeEvent(event.type, event.data.toString()), event.id)
                }.entries
                if (generation == streamGeneration) mutable.update {
                    it.copy(entries = prependTranscript(historical, it.entries), historyBefore = page.before, hasOlder = page.hasOlder)
                }
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { if (generation == streamGeneration) mutable.update { it.copy(historyError = e.userMessage()) } }
            finally { if (generation == streamGeneration) mutable.update { it.copy(loadingOlder = false) } }
        }
    }

    fun resume() {
        if (mutable.value.busy || mutable.value.session?.state != SessionState.Closed || mutable.value.conversation?.resumeAvailable != true || mutable.value.conversation?.deleting == true) return
        mutable.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            try {
                val session = api.resume(id)
                disconnect()
                lastEventId = null
                mutable.update { it.copy(session = session, busy = false) }
                connect()
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.update { it.copy(busy = false, error = e.userMessage()) } }
        }
    }

    fun deleteConversation() {
        if (mutable.value.busy || mutable.value.session?.state != SessionState.Closed) return
        mutable.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            try {
                api.deleteConversation(id)
                disconnect()
                mutable.update { it.copy(busy = false, deleted = true) }
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { mutable.update { it.copy(busy = false, error = e.userMessage()) } }
        }
    }

    fun send(input: String, onSent: () -> Unit) {
        if (!mutable.value.canSend) return
        action {
            val revision = mutable.value.eventRevision
            val selected = requireNotNull(mutable.value.turnSettings)
            val session = requireNotNull(mutable.value.session)
            val pending = session.pendingInput
            require(pending == null || input == pending.input) { "Envie a primeira mensagem pendente antes de outra mensagem." }
            val turn = api.turn(session.id, input, selected, signer.signTurn(session, input), pending?.id)
            // A retry may acknowledge a turn that already finished. Read its
            // current state rather than marking that old turn as running again.
            val confirmed = if (pending != null) api.session(session.id) else null
            mutable.update {
                val acknowledged = it.session?.copy(pendingInput = null)
                it.copy(selectedSettings = null, session = when {
                    it.eventRevision != revision -> acknowledged
                    confirmed != null -> confirmed
                    else -> acknowledged?.copy(state = SessionState.Running, turnId = turn.turnId,
                        model = selected.model, reasoningEffort = selected.reasoningEffort)
                })
            }
            onSent()
        }
    }
    fun cancel() = action { api.cancel(requireNotNull(mutable.value.session).id) }
    fun close() = action {
        val revision = mutable.value.eventRevision
        api.close(requireNotNull(mutable.value.session).id)
        mutable.update { if (it.eventRevision == revision) it.copy(session = it.session?.copy(state = SessionState.Closing)) else it }
    }
    fun decide(approvalId: String, decision: Decision) = action {
        val approval = mutable.value.approvals.firstOrNull { it.approvalId == approvalId }
        require(approval != null) { "A aprovação não está pendente." }
        val session = requireNotNull(mutable.value.session)
        api.decide(session.id, approvalId, decision, signer.signApproval(session.id, approval, decision))
        // Only the authoritative SSE event removes the pending approval.
    }

    // A new session's first prompt waits until the runner can take it as a signed turn.
    private fun sendPendingPrompt() {
        val session = mutable.value.session ?: return
        if (!mutable.value.canSend) return
        val pending = session.pendingInput ?: return
        send(pending.input) {}
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

fun prependTranscript(older: List<TranscriptEntry>, current: List<TranscriptEntry>): List<TranscriptEntry> {
    val last = older.lastOrNull()
    val first = current.firstOrNull()
    return if (last != null && first != null && last.turnId == first.turnId && last.kind == first.kind &&
        first.kind in listOf("Text", "Thinking", "ToolCall", "ToolResult")) {
        older.dropLast(1) + first.copy(content = last.content + first.content) + current.drop(1)
    } else older + current
}
