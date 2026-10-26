package com.gustavomartins.codexpool.data

import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp
import java.net.Proxy
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.util.concurrent.TimeUnit
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSession
import javax.net.ssl.X509TrustManager
import okhttp3.ConnectionSpec
import okhttp3.CookieJar
import okhttp3.OkHttpClient

/**
 * Bootstrap clients for Android. Each QR gets its own OkHttp client whose only
 * trusted certificate is the one the QR pins; the pool client and the system
 * trust store are never involved, and no request leaves before the pinned
 * handshake succeeds.
 */
class OkHttpBootstrapClients : BootstrapClientFactory {
    override fun create(payload: BootstrapPayload): HttpClient = HttpClient(OkHttp) {
        engine { preconfigured = bootstrapOkHttpClient(payload) }
        followRedirects = false
        expectSuccess = false
    }
}

internal fun bootstrapOkHttpClient(payload: BootstrapPayload): OkHttpClient {
    val hosts = payload.endpoints.mapNotNull(::bootstrapEndpointHost).toSet()
    val trust = PinnedBootstrapTrust(payload.tlsSpkiSha256)
    val tls = SSLContext.getInstance("TLS").apply { init(null, arrayOf(trust), null) }
    return OkHttpClient.Builder()
        .sslSocketFactory(tls.socketFactory, trust)
        .hostnameVerifier { host, session -> host in hosts && trust.coversHost(session, host) }
        .connectionSpecs(listOf(ConnectionSpec.MODERN_TLS))
        .followRedirects(false)
        .followSslRedirects(false)
        .cookieJar(CookieJar.NO_COOKIES)
        .proxy(Proxy.NO_PROXY)
        .connectTimeout(5, TimeUnit.SECONDS)
        .readTimeout(60, TimeUnit.SECONDS)
        .callTimeout(75, TimeUnit.SECONDS)
        .build()
}

/** Trusts exactly one certificate: the unexpired leaf whose SPKI hash matches the QR. */
internal class PinnedBootstrapTrust(private val pin: String) : X509TrustManager {
    override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
        val leaf = chain?.firstOrNull() ?: throw CertificateException("Certificado ausente.")
        leaf.checkValidity()
        if (!bootstrapPinMatches(leaf.publicKey.encoded, pin)) throw CertificateException("O certificado do computador não corresponde ao QR.")
    }

    override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) =
        throw CertificateException("Certificados de cliente não são aceitos.")

    override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()

    /** The pinned leaf must also name the IP address the app dialed. */
    fun coversHost(session: SSLSession, host: String): Boolean {
        val leaf = runCatching { session.peerCertificates.firstOrNull() as? X509Certificate }.getOrNull() ?: return false
        if (!bootstrapPinMatches(leaf.publicKey.encoded, pin)) return false
        return leaf.subjectAlternativeNames.orEmpty().any { it.getOrNull(0) == 7 && it.getOrNull(1) == host }
    }
}
