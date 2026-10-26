@file:OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)

package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.*
import kotlinx.coroutines.test.*
import kotlin.test.*

class RunnerCatalogTest {
    private val credentials = Credentials("https://pool.example", "token", "client-a")
    private fun snapshot(seed: Char, model: String = "old") = RunnerCatalogSnapshot(seed.toString().repeat(64),
        listOf(CatalogModel(model, protocol = "responses", availableNow = true)),
        listOf(Runner("runner", "machine", online = true, agents = listOf(Agent.Codex), modelsKnown = true,
            models = listOf(AgentModel(Agent.Codex, model)))))

    private inner class Source(var current: RunnerCatalogSnapshot) : RunnerCatalogSource {
        override val credentials = this@RunnerCatalogTest.credentials
        val updates = MutableSharedFlow<RunnerCatalogSnapshot>()
        var fail = false
        val requested = mutableListOf<String?>()
        val cursors = mutableListOf<String?>()
        override suspend fun runnerCatalog(revision: String?): RunnerCatalogSnapshot? {
            requested += revision
            if (fail) throw PoolApiException(503, "/api/me/runner-catalog")
            return current.takeIf { revision != it.revision }
        }
        override fun runnerCatalogEvents(revision: String?) = flow {
            cursors += revision
            emitAll(updates)
        }
    }

    @Test fun cacheStartsOfflineAndOnlyFreshResponsesMakeModelsSelectable() = runTest {
        val initial = snapshot('a')
        val source = Source(initial)
        val persisted = mutableListOf<RunnerCatalogSnapshot>()
        val repository = RunnerCatalogRepository(source, RunnerCatalogCache(credentials.baseUrl, credentials.clientId, initial), persist = { persisted += it })
        assertFalse(repository.state.value.fresh)
        assertFalse(repository.state.value.runners.single().online)
        repository.start(backgroundScope); runCurrent()
        assertTrue(repository.state.value.fresh)
        assertTrue(repository.state.value.runners.single().online)
        assertEquals(listOf<String?>(initial.revision), source.requested)
        assertEquals(listOf<String?>(initial.revision), source.cursors)
        assertTrue(persisted.isEmpty())
        val next = snapshot('b', "new")
        source.current = next
        source.updates.emit(next); runCurrent()
        assertEquals(next, repository.state.value.snapshot)
        assertEquals(listOf(next), persisted)
        repository.stop()
        assertFalse(repository.state.value.runners.single().online)
    }

    @Test fun refreshResynchronizesAfterLostEventsAndPersistsOnlyChangedRevisions() = runTest {
        val source = Source(snapshot('a'))
        val persisted = mutableListOf<RunnerCatalogSnapshot>()
        val repository = RunnerCatalogRepository(source, persist = { persisted += it })
        repository.start(backgroundScope); runCurrent()
        val missed = snapshot('c', "after-offline")
        source.current = missed
        repository.refresh()
        assertEquals(missed, repository.state.value.snapshot)
        repository.refresh()
        assertEquals(2, persisted.size)
        assertEquals(missed.revision, source.cursors.last())
        repository.stop()
    }

    @Test fun failureNeverUsesCachedOnlineFlagsAndRestartRevalidatesThem() = runTest {
        val source = Source(snapshot('a')).apply { fail = true }
        val repository = RunnerCatalogRepository(source, RunnerCatalogCache(credentials.baseUrl, credentials.clientId, source.current))
        repository.start(backgroundScope); runCurrent()
        assertFalse(repository.state.value.fresh)
        assertFalse(repository.state.value.runners.single().online)
        source.fail = false
        advanceTimeBy(501); runCurrent()
        assertTrue(repository.state.value.fresh)
        repository.stop()
    }

    @Test fun cachedCatalogsStayWithTheirAccountAndAreClearedOnLogout() {
        val other = Credentials("https://other.example", "other-token", "client-b")
        val accounts = PoolAccounts(selectedUrl = other.baseUrl, providers = listOf(
            PoolProvider("A", credentials.baseUrl, credentials), PoolProvider("B", other.baseUrl, other)))
        val updated = accounts.withRunnerCatalog(snapshot('a'), credentials)
        assertNotNull(updated.providers.first().runnerCatalog)
        assertNull(updated.selected.runnerCatalog)
        assertEquals(updated, decodeStoredAccounts(PoolJson.encodeToString(updated)).accounts)
        assertEquals(updated, updated.withRunnerCatalog(snapshot('b'), credentials.copy(clientId = "different-account")))
        val loggedOut = updated.copy(selectedUrl = credentials.baseUrl).withCredentials(null)
        assertNull(loggedOut.selected.runnerCatalog)
        val repository = RunnerCatalogRepository(Source(snapshot('b')), RunnerCatalogCache(credentials.baseUrl, "another-client", snapshot('a')))
        assertNull(repository.state.value.snapshot)
        val foreignPool = RunnerCatalogRepository(Source(snapshot('b')),
            RunnerCatalogCache("https://foreign.example", credentials.clientId, snapshot('a')))
        assertNull(foreignPool.state.value.snapshot)
    }

    @Test fun existingSessionsKeepTheirOwnCapabilitySnapshot() {
        val original = snapshot('a')
        val global = snapshot('b', "new")
        val capabilities = ModelCapabilities("1".repeat(64), original.runners.single().models)
        val session = Session("session", Agent.Codex, "old", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated,
            SessionState.WaitingInput, runnerId = "runner", conversationId = "conversation", capabilities = capabilities)
        val current = RunnerCatalogState(global.copy(models = original.models + global.models), fresh = true)
        assertEquals(listOf("old"), current.modelsForSession(session).map { it.id })
        assertTrue(current.copy(fresh = false).modelsForSession(session).isEmpty())
    }

    @Test fun cancellingTheOwnerInvalidatesLiveState() = runTest {
        val source = Source(snapshot('a'))
        val repository = RunnerCatalogRepository(source)
        val owner = CoroutineScope(backgroundScope.coroutineContext + Job())
        repository.start(owner); runCurrent()
        assertTrue(repository.state.value.fresh)
        owner.cancel(); runCurrent()
        assertFalse(repository.state.value.fresh)
        repository.stop()
    }

    @Test fun cacheWriteFailureDoesNotHideAuthoritativeOnlineState() = runTest {
        val source = Source(snapshot('a'))
        var fail = true
        var writes = 0
        val repository = RunnerCatalogRepository(source, persist = { writes++; if (fail) error("disk full") })
        repository.start(backgroundScope); runCurrent()
        assertTrue(repository.state.value.fresh)
        assertNotNull(repository.state.value.error)
        fail = false
        repository.refresh()
        assertTrue(repository.state.value.fresh)
        assertEquals(2, writes)
        repository.stop()
    }
}
