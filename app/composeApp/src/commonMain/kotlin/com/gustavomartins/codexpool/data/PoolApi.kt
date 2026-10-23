package com.gustavomartins.codexpool.data

import io.ktor.client.HttpClient
import io.ktor.client.HttpClientConfig
import io.ktor.client.call.body
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.client.plugins.sse.SSE
import io.ktor.client.plugins.sse.sse
import io.ktor.client.request.*
import io.ktor.client.statement.HttpResponse
import io.ktor.http.*
import io.ktor.serialization.kotlinx.json.json
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow

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
    install(SSE) { maxReconnectionAttempts = 0 }
}

class PoolApiException(val status: Int, val endpoint: String) : Exception(when (status) {
    401 -> "Autenticação recusada pelo pool (401). Entre novamente."
    403 -> "Acesso negado pelo pool (403)."
    404 -> "O pool não disponibiliza $endpoint (404)."
    else -> "O pool recusou $endpoint (HTTP $status)."
})

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
    suspend fun enroll(): Enrollment = client.post(endpoint("/api/me/runners")) { authorize() }
        .checked("/api/me/runners").body()
    suspend fun revoke(id: String) { client.delete(endpoint("/api/me/runners/${resourceId(id)}")) { authorize() }.checked("/api/me/runners") }
    suspend fun catalog(): List<CatalogModel> = client.get(endpoint("/api/pool/catalog")) { authorize() }
        .checked("/api/pool/catalog").body<Catalog>().models
    suspend fun quotas(): Quotas = client.get(endpoint("/api/me/quotas")) { authorize() }.checked("/api/me/quotas").body()

    fun events(id: String, onConnected: () -> Unit): Flow<SessionEvent> = flow {
        client.sse(request = { url(endpoint("${sessionPath(id)}/events")); authorize() }) {
            onConnected()
            incoming.collect { event ->
                val data = event.data
                if (data != null) {
                    val type = requireNotNull(event.event) { "Evento SSE sem tipo." }
                    emit(decodeEvent(type, data))
                }
            }
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
