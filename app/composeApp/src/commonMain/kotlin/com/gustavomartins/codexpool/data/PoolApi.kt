package com.gustavomartins.codexpool.data

import io.ktor.client.HttpClient
import io.ktor.client.HttpClientConfig
import io.ktor.client.call.body
import io.ktor.client.plugins.HttpTimeout
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.client.plugins.sse.SSE
import io.ktor.client.plugins.sse.SSEClientException
import io.ktor.client.plugins.sse.sse
import io.ktor.client.request.*
import io.ktor.client.statement.HttpResponse
import io.ktor.http.*
import io.ktor.serialization.kotlinx.json.json
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.flow.transformWhile
import kotlinx.coroutines.delay
import kotlinx.coroutines.withTimeoutOrNull

interface CredentialStore {
    suspend fun read(): PoolAccounts?
    suspend fun write(accounts: PoolAccounts)
    suspend fun clear()
}

fun normalizePoolUrl(value: String): String {
    val url = Url(value.trim())
    require(url.protocol == URLProtocol.HTTPS && url.host.isNotBlank()) { "Informe uma URL HTTPS válida para o pool." }
    require(url.user.isNullOrEmpty() && url.password.isNullOrEmpty() && url.parameters.isEmpty() && url.fragment.isEmpty()) {
        "A URL do pool não pode conter credenciais, parâmetros ou fragmentos."
    }
    require(url.encodedPath == "/" || url.encodedPath.isEmpty()) { "Informe somente a origem do pool, sem caminho." }
    return url.toString().trimEnd('/')
}

fun HttpClientConfig<*>.poolConfiguration() {
    followRedirects = false
    expectSuccess = false
    install(ContentNegotiation) { json(PoolJson) }
    install(HttpTimeout) {
        connectTimeoutMillis = 15_000
        requestTimeoutMillis = 30_000
        socketTimeoutMillis = 45_000
    }
    install(SSE) { maxReconnectionAttempts = 0 }
}

class PoolApiException(val status: Int, val endpoint: String) : Exception(when (status) {
    401 -> "Autenticação recusada pelo pool (401). Entre novamente."
    403 -> "Acesso negado pelo pool (403)."
    404 -> "O pool não disponibiliza $endpoint (404)."
    410 -> "O pedido de pareamento expirou. Gere um novo QR no computador."
    429 -> "Muitas tentativas. Aguarde antes de tentar novamente."
    409 -> if (endpoint.endsWith("/events")) "O cursor de eventos expirou. Não foi possível recuperar os eventos pendentes."
        else if (endpoint.endsWith("/resume")) "Não foi possível retomar. Conecte o runner original e verifique se o ponto de retomada está disponível."
        else if (endpoint.endsWith("/history")) "O histórico desta sessão não está disponível ou o cursor é inválido. Sessões anteriores à gravação do histórico não podem ser recuperadas."
        else if (endpoint.contains("runner-pairings")) "Encerre as sessões desse runner antes de rotacionar sua credencial."
        else "O estado da sessão mudou no pool (409). Atualize e tente novamente."
    else -> "O pool recusou $endpoint (HTTP $status)."
})

class SessionProtocolException(message: String) : Exception(message)

class PoolApi(private val client: HttpClient, val credentials: Credentials) {
    init { require(normalizePoolUrl(credentials.baseUrl) == credentials.baseUrl); require(credentials.token.isNotBlank()) }

    companion object {
        suspend fun login(client: HttpClient, url: String, username: String, password: String): Credentials {
            val base = normalizePoolUrl(url)
            require(username.isNotBlank() && password.isNotEmpty()) { "Informe usuário e senha." }
            val response = client.post("$base/api/auth/token") {
                contentType(ContentType.Application.Json)
                setBody(LoginRequest(username.trim(), password))
            }.checked("/api/auth/token").body<LoginResponse>()
            require(response.token.isNotBlank() && response.clientId.isNotBlank()) { "O pool retornou uma credencial inválida." }
            return Credentials(base, response.token, response.clientId)
        }
    }

    private fun endpoint(path: String) = credentials.baseUrl + path
    private fun HttpRequestBuilder.authorize() { bearerAuth(credentials.token) }
    private fun sessionPath(id: String): String = "/api/sessions/${resourceId(id)}"
    private fun conversationPath(id: String): String = "/api/conversations/${resourceId(id)}"
    suspend fun conversation(id: String): Conversation = client.get(endpoint(conversationPath(id))) { authorize() }
        .checked(conversationPath(id)).body<Conversation>().also { require(it.id == id) { "O pool retornou outra conversa." } }
    suspend fun resume(id: String): Session = client.post(endpoint("${conversationPath(id)}/resume")) { authorize() }
        .checked("${conversationPath(id)}/resume").body<Session>().also { require(it.conversationId == id) { "O pool retomou outra conversa." } }
    suspend fun deleteConversation(id: String) {
        val path = conversationPath(id)
        val response = client.delete(endpoint(path)) { authorize() }.checked(path)
        require(response.status == HttpStatusCode.Accepted) { "O pool retornou um estado de exclusão incompatível." }
        val deleted = withTimeoutOrNull(30_000) {
            while (true) {
                try {
                    val conversation = conversation(id)
                    conversation.deleteError?.let { throw SessionProtocolException(it) }
                } catch (e: PoolApiException) { if (e.status == 404) return@withTimeoutOrNull true else throw e }
                delay(500)
            }
            @Suppress("UNREACHABLE_CODE") false
        }
        if (deleted != true) throw SessionProtocolException("A exclusão ainda está em andamento. Atualize a lista para acompanhar.")
    }
    suspend fun conversations(storage: ConversationStorage, cursor: String? = null): ConversationPage = client.get(endpoint("/api/conversations")) {
        authorize(); parameter("storage", if (storage == ConversationStorage.Active) "active" else "archived"); parameter("limit", 50)
        cursor?.let { parameter("cursor", it) }
    }.checked("/api/conversations").body<ConversationPage>().also { page ->
        require(page.conversations.all { it.storage == storage } && page.conversations.map { it.id }.distinct().size == page.conversations.size) {
            "O pool retornou uma lista de conversas inconsistente."
        }
    }
    suspend fun session(id: String): Session = client.get(endpoint(sessionPath(id))) { authorize() }
        .checked(sessionPath(id)).body()
    suspend fun history(id: String, before: Long? = null, through: Long? = null): HistoryPage {
        val path = "${conversationPath(id)}/history"
        val page = client.get(endpoint(path)) {
            authorize(); parameter("limit", 256)
            before?.let { parameter("before", it) }; through?.let { parameter("through", it) }
        }.checked(path).body<HistoryPage>()
        require(page.conversation.id == id && page.session.id == page.conversation.sessionId && page.session.conversationId == id &&
            page.through >= 0 && (through == null || through == page.through)) { "O pool retornou um histórico de conversa inconsistente." }
        val end = before ?: (page.through + 1)
        require(end in 1..(page.through + 1) && page.before in 1..end && page.hasOlder == (page.before > 1)) { "A paginação do histórico é inválida." }
        require(page.events.size.toLong() == end - page.before) { "O histórico contém uma lacuna." }
        page.events.forEachIndexed { index, event -> require(event.id == page.before + index) { "O histórico contém eventos fora de ordem." } }
        require(!page.hasOlder || page.events.isNotEmpty()) { "A página de histórico está incompleta." }
        return page
    }
    suspend fun create(request: CreateSession): Session {
        request.validate()
        return client.post(endpoint("/api/sessions")) {
            authorize(); contentType(ContentType.Application.Json); setBody(request)
        }.checked("/api/sessions").body()
    }
    suspend fun turn(id: String, input: String, settings: TurnSettings? = null): TurnStarted {
        require(input.isNotBlank()) { "O turno não pode estar vazio." }
        return client.post(endpoint("${sessionPath(id)}/turns")) {
            authorize(); contentType(ContentType.Application.Json); setBody(TurnInput(input, settings?.model, settings?.reasoningEffort))
        }.checked("${sessionPath(id)}/turns").body()
    }
    suspend fun cancel(id: String) { client.post(endpoint("${sessionPath(id)}/turns:cancel")) { authorize() }.checked(sessionPath(id)) }
    suspend fun close(id: String) { client.delete(endpoint(sessionPath(id))) { authorize() }.checked(sessionPath(id)) }
    suspend fun decide(id: String, approvalId: String, decision: Decision) {
        client.post(endpoint("${sessionPath(id)}/approvals/${resourceId(approvalId)}")) {
            authorize(); contentType(ContentType.Application.Json); setBody(ApprovalDecision(decision))
        }.checked("${sessionPath(id)}/approvals")
    }
    suspend fun runners(): List<Runner> = client.get(endpoint("/api/me/runners")) { authorize() }
        .checked("/api/me/runners").body<RunnerList>().runners
    suspend fun revoke(id: String) { client.delete(endpoint("/api/me/runners/${resourceId(id)}")) { authorize() }.checked("/api/me/runners") }
    suspend fun reviewPairing(link: String): PairingReview {
        val id = pairingRequestId(link, credentials.baseUrl)
        return client.get(endpoint("/api/me/runner-pairings/$id")) { authorize() }
            .checked("/api/me/runner-pairings/$id").body<PairingReview>().also {
                require(it.id == id) { "O pool retornou outro pedido de pareamento." }
            }
    }
    suspend fun decidePairing(id: String, decision: Decision, code: String) {
        require(decision == Decision.Deny || code.matches(Regex("[A-Za-z0-9_-]{8}"))) { "Informe o c?digo exibido no computador." }
        client.post(endpoint("/api/me/runner-pairings/${resourceId(id)}")) {
            authorize(); contentType(ContentType.Application.Json); setBody(PairingDecision(decision, code))
        }.checked("/api/me/runner-pairings/$id")
    }
    suspend fun catalog(): List<CatalogModel> = client.get(endpoint("/api/pool/catalog")) { authorize() }
        .checked("/api/pool/catalog").body<Catalog>().models
    suspend fun quotas(): Quotas = client.get(endpoint("/api/me/quotas")) { authorize() }.checked("/api/me/quotas").body()
    suspend fun logout() { client.delete(endpoint("/api/auth/token")) { authorize() }.checked("/api/auth/token") }

    fun events(id: String, lastEventId: String? = null, onCursor: (String) -> Unit = {}, onConnected: () -> Unit): Flow<SequencedSessionEvent> = flow {
        var cursor = lastEventId?.toLong()
        val path = "${conversationPath(id)}/events"
        try {
            client.sse(request = {
                url(endpoint(path)); authorize()
                lastEventId?.let { header("Last-Event-ID", it) }
            }) {
                onConnected()
                incoming.transformWhile { event ->
                    val data = event.data
                    if (data != null) {
                        val type = requireNotNull(event.event) { "Evento SSE sem tipo." }
                        val next = event.id?.let { raw ->
                            val value = requireNotNull(raw.toLongOrNull()) { "Cursor SSE inválido." }
                            require(value > 0 && (cursor == null || value == cursor!! + 1)) { "Cursor SSE fora de ordem ou com lacuna." }
                            value
                        }
                        val decoded = decodeEvent(type, data)
                        emit(SequencedSessionEvent(next, decoded))
                        if (next != null) { cursor = next; onCursor(next.toString()) }
                        decoded !is SessionEvent.Closed && decoded !is SessionEvent.Error
                    } else true
                }.collect { emit(it) }
            }
        } catch (e: SSEClientException) {
            val response = e.response
            if (response != null && response.status.value != 200) throw PoolApiException(response.status.value, path)
            if (response != null && response.contentType()?.withoutParameters() != ContentType.Text.EventStream) {
                throw SessionProtocolException("O pool retornou um canal de eventos incompatível.")
            }
            throw (e.cause ?: SessionProtocolException("O canal SSE falhou sem um motivo de transporte."))
        }
    }
}

private fun resourceId(value: String): String {
    require(value.matches(Regex("[A-Za-z0-9_-]+"))) { "Identificador de recurso inválido." }
    return value
}

private fun HttpResponse.checked(endpoint: String): HttpResponse {
    if (status.value !in 200..299) throw PoolApiException(status.value, endpoint)
    return this
}
