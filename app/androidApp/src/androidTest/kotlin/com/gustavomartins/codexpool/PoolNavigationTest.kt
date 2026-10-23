package com.gustavomartins.codexpool

import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.PoolApp
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import org.junit.After
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class PoolNavigationTest {
    @get:Rule val compose = createComposeRule()
    private val client = HttpClient(MockEngine { request ->
        val body = when (request.url.encodedPath) {
            "/api/sessions" -> """{"sessions":[]}"""
            "/api/pool/catalog" -> """{"models":[{"id":"test-model","protocol":"openai","available_now":true}]}"""
            "/api/me/runners" -> """{"runners":[]}"""
            "/api/me/quotas" -> """{"models":[],"generated_at":"2026-10-23T00:00:00Z"}"""
            else -> error("Unexpected endpoint in UI test: ${request.url.encodedPath}")
        }
        respond(body, HttpStatusCode.OK, headersOf(HttpHeaders.ContentType, "application/json"))
    }) { poolConfiguration() }
    private val store = object : CredentialStore {
        override suspend fun read() = Credentials("https://pool.example", "ui-test-token", "test-client")
        override suspend fun write(credentials: Credentials) {}
        override suspend fun clear() {}
    }

    @After fun closeClient() { client.close() }

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

    private fun waitForText(value: String) {
        compose.waitUntil(10_000) { compose.onAllNodesWithText(value).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText(value).assertIsDisplayed()
    }
}
