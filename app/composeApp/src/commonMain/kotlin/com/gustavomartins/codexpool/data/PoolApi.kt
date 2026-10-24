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

interface CredentialStore {
    suspend fun read(): Credentials?
    suspend fun write(credentials: Credentials)
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
    suspend fun sessions(): List<Session> = client.get(endpoint("/api/sessions")) { authorize() }
        .checked("/api/sessions").body<SessionList>().sessions
    suspend fun session(id: String): Session = client.get(endpoint(sessionPath(id))) { authorize() }
        .checked(sessionPath(id)).body()
    suspend fun create(request: CreateSession): Session {
        request.validate()
        return client.post(endpoint("/api/sessions")) {
            authorize(); contentType(ContentType.Application.Json); setBody(request)
        }.checked("/api/sessions").body()
    }
    suspend fun turn(id: String, input: String): TurnStarted {
        require(input.isNotBlank()) { "O turno não pode estar vazio." }
        return client.post(endpoint("${sessionPath(id)}/turns")) {
            authorize(); contentType(ContentType.Application.Json); setBody(TurnInput(input))
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

    fun events(id: String, lastEventId: String? = null, onCursor: (String) -> Unit = {}, onConnected: () -> Unit): Flow<SessionEvent> = flow {
        var cursor = lastEventId?.toLong()
        val path = "${sessionPath(id)}/events"
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
                            require(value > 0 && (cursor == null || value > cursor!!)) { "Cursor SSE fora de ordem." }
                            value
                        }
                        val decoded = decodeEvent(type, data)
                        emit(decoded)
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
