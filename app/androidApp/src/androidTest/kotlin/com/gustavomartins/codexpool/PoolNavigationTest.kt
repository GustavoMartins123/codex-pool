package com.gustavomartins.codexpool

import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.v2.createComposeRule
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.PoolApp
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import io.ktor.http.content.TextContent
import org.junit.After
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.koin.core.context.stopKoin

@RunWith(AndroidJUnit4::class)
class PoolNavigationTest {
    @get:Rule val compose = createComposeRule()
    private val client = HttpClient(MockEngine { request ->
        if (request.url.encodedPath == "/api/auth/token" && request.method == HttpMethod.Delete) {
            respond("", HttpStatusCode.NoContent)
        } else {
            val body = when (request.url.encodedPath) {
                "/api/me/runner-pairings/" + "a".repeat(43) -> {
                    if (request.method == HttpMethod.Post) {
                        val decision = PoolJson.decodeFromString<PairingDecision>((request.body as TextContent).text)
                        check(decision == PairingDecision(Decision.Approve, "AbCd1234"))
                        return@MockEngine respond("", HttpStatusCode.NoContent)
                    }
                    """{"id":"${"a".repeat(43)}","name":"Synthetic machine","os":"test","arch":"test","expires_at":"2026-10-24T04:00:00Z"}"""
                }
                "/api/auth/token" -> {
                    check(PoolJson.decodeFromString<LoginRequest>((request.body as TextContent).text) == LoginRequest("mobile-user", "test-password"))
                    """{"token":"ui-test-token","client_id":"test-client"}"""
                }
                "/api/conversations" -> """{"conversations":[]}"""
                "/api/pool/catalog" -> """{"models":[{"id":"test-model","protocol":"openai","available_now":true}]}"""
                "/api/me/runners" -> """{"runners":[]}"""
                "/api/me/quotas" -> """{"models":[],"generated_at":"2026-10-23T00:00:00Z"}"""
                else -> error("Unexpected endpoint in UI test: ${request.url.encodedPath}")
            }
            respond(body, HttpStatusCode.OK, headersOf(HttpHeaders.ContentType, "application/json"))
        }
    }) { poolConfiguration() }
    private val store = object : CredentialStore {
        override suspend fun read() = PoolAccounts.initial(AppBuildConfig.PoolUrl).withCredentials(Credentials(AppBuildConfig.PoolUrl, "ui-test-token", "test-client"))
        override suspend fun write(accounts: PoolAccounts) {}
        override suspend fun clear() {}
    }

    @After fun closeClient() {
        PairingLinks.clear()
        stopKoin()
        client.close()
    }

    @Test fun pairingRequiresReviewAndCodeBeforeApproval() {
        compose.setContent { PoolApp(store, client) }
        waitForText("Nenhuma sessão")
        compose.onNodeWithText("Runners").performClick()
        waitForText("Nenhum runner conectado")
        compose.onNodeWithText("Conectar runner", useUnmergedTree = true).performClick()
        waitForText("Conectar máquina")
        val link = "codexpool://pair?pool=${AppBuildConfig.PoolUrl.encodeURLParameter()}&request=${"a".repeat(43)}"
        compose.onNodeWithText("Link de pareamento").performScrollTo().performTextInput(link)
        compose.onNodeWithText("Verificar máquina").performScrollTo().performClick()
        waitForText("Synthetic machine")
        compose.onNodeWithText("Autorizar máquina").performScrollTo().assertIsNotEnabled()
        compose.onNodeWithText("Código no computador").performScrollTo().performTextInput("AbCd1234")
        compose.onNodeWithText("Autorizar máquina").performScrollTo().assertIsEnabled().performClick()
        waitForText("Decisão enviada")
        compose.onNodeWithText("Concluir").performClick()
    }

    @Test fun scannedLinkReturnsFromSessionCreationToMachineReview() {
        compose.setContent { PoolApp(store, client) }
        waitForText("Nenhuma sessão")
        compose.onNodeWithText("+ Nova sessão").performClick()
        waitForText("Escolha como trabalhar")
        compose.runOnIdle {
            PairingLinks.receive("codexpool://pair?pool=${AppBuildConfig.PoolUrl.encodeURLParameter()}&request=${"a".repeat(43)}")
        }
        waitForText("Synthetic machine")
        compose.onNodeWithText("Autorizar máquina").assertIsNotEnabled()
        compose.onNodeWithText("Código no computador").assertIsDisplayed()
    }


    @Test fun authenticatedNavigationAndPathPermissionWorkOnDevice() {
        compose.setContent { PoolApp(store, client) }
        waitForText("Nenhuma sessão")
        compose.onNodeWithText("+ Nova sessão").performClick()
        waitForText("Escolha como trabalhar")
        compose.onNodeWithText("Caminho").performScrollTo().performClick()
        compose.onNodeWithText("Obrigatório para workspace por caminho").performScrollTo().assertIsDisplayed()
        compose.onNode(isToggleable()).assertIsOn().assertIsNotEnabled()
        compose.onNodeWithText("Voltar").performClick()
        compose.onNodeWithText("Runners").performClick()
        waitForText("Nenhum runner conectado")
        compose.onNodeWithText("Quotas").performClick()
        waitForText("Sem quotas disponíveis")
    }

    @Test fun loginUsesWebsiteFieldsAndBuildOriginThenLogoutClearsCredential() {
        var saved: PoolAccounts? = null
        val signedOutStore = object : CredentialStore {
            override suspend fun read() = saved
            override suspend fun write(accounts: PoolAccounts) { saved = accounts }
            override suspend fun clear() { saved = null }
        }
        compose.setContent { PoolApp(signedOutStore, client) }
        waitForText("Usuário ou e-mail")
        compose.onNodeWithText("URL do pool").assertDoesNotExist()
        compose.onNodeWithText("Usuário ou e-mail").performTextInput("mobile-user")
        compose.onNodeWithText("Senha").performTextInput("test-password")
        compose.onNodeWithText("Entrar no pool").performScrollTo().performClick()
        waitForText("Nenhuma sessão")
        check(saved?.selectedUrl == AppBuildConfig.PoolUrl)
        compose.onNodeWithText("Conta").performClick()
        compose.onNodeWithText("Sair").performClick()
        waitForText("Entre com a mesma conta do site.")
        check(saved?.selected?.credentials == null)
    }

    @Test fun providerDomainsStayInManagementAndSwitchingRestoresEachLogin() {
        compose.setContent { PoolApp(store, client) }
        waitForText("Nenhuma sessão")
        compose.onNodeWithText(AppBuildConfig.PoolUrl).assertDoesNotExist()
        compose.onNodeWithText("Conta").performClick()
        compose.onNodeWithText("Provedores").performClick()
        waitForText("Adicionar provedor")
        compose.onNodeWithText(AppBuildConfig.PoolUrl).assertIsDisplayed()
        compose.onNodeWithText("Nome do provedor").performScrollTo().performTextInput("Secondary")
        compose.onNodeWithText("Domínio HTTPS").performScrollTo().performTextInput("https://secondary.example")
        compose.onNodeWithText("Adicionar e selecionar").performScrollTo().performClick()
        compose.onNodeWithText("Concluir").performScrollTo().performClick()
        waitForText("Provedor: Secondary")
        compose.onNodeWithText("Usuário ou e-mail").performScrollTo().performTextInput("mobile-user")
        compose.onNodeWithText("Senha").performScrollTo().performTextInput("test-password")
        compose.onNodeWithText("Entrar no pool").performScrollTo().performClick()
        waitForText("Nenhuma sessão")
        compose.onNodeWithText("https://secondary.example").assertDoesNotExist()
        check(client.engine is MockEngine)
        val engine = client.engine as MockEngine
        check(engine.requestHistory.any { it.url.host == "secondary.example" && it.url.encodedPath == "/api/auth/token" })
        compose.onNodeWithText("Conta").performClick()
        compose.onNodeWithText("Provedores").performClick()
        compose.onNodeWithText("Principal").performScrollTo().performClick()
        compose.onNodeWithText("Concluir").performScrollTo().performClick()
        waitForText("Nenhuma sessão")
        compose.onNodeWithText("Principal").assertIsDisplayed()
        compose.onNodeWithText(AppBuildConfig.PoolUrl).assertDoesNotExist()
    }

    private fun waitForText(value: String) {
        try {
            compose.waitUntil(10_000) { compose.onAllNodesWithText(value).fetchSemanticsNodes().isNotEmpty() }
        } catch (failure: Throwable) {
            throw AssertionError("Expected '$value'. UI: ${compose.onAllNodes(isRoot(), useUnmergedTree = true).printToString()}", failure)
        }
        compose.onNodeWithText(value).assertIsDisplayed()
    }
}
