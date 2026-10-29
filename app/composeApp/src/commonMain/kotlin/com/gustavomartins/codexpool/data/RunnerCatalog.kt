package com.gustavomartins.codexpool.data

import kotlinx.coroutines.*
import kotlinx.coroutines.flow.*
import kotlinx.serialization.Serializable

@Serializable
data class RunnerCatalogSnapshot(val revision: String, val models: List<CatalogModel>, val runners: List<Runner>) {
    fun validate(): RunnerCatalogSnapshot {
        require(revision.matches(Regex("[a-f0-9]{64}")) && models.size <= 512 && runners.size <= 1024) { "Catálogo de runners inválido." }
        require(models.map { it.id }.distinct().size == models.size && runners.map { it.id }.distinct().size == runners.size) { "Catálogo de runners duplicado." }
        return this
    }
}

@Serializable
data class RunnerCatalogCache(val baseUrl: String, val clientId: String, val snapshot: RunnerCatalogSnapshot)

interface RunnerCatalogSource {
    val credentials: Credentials
    suspend fun runnerCatalog(revision: String? = null): RunnerCatalogSnapshot?
    fun runnerCatalogEvents(revision: String?): Flow<RunnerCatalogSnapshot>
}

data class RunnerCatalogState(val snapshot: RunnerCatalogSnapshot? = null, val fresh: Boolean = false, val error: String? = null) {
    // Cached online flags are never evidence that a runner is still connected.
    val runners get() = snapshot?.runners.orEmpty().map { if (fresh) it else it.copy(online = false) }
}

class RunnerCatalogRepository(
    private val api: RunnerCatalogSource,
    cached: RunnerCatalogCache? = null,
    private val persist: suspend (RunnerCatalogSnapshot) -> Unit = {},
) {
    private val initial = cached?.takeIf { it.baseUrl == api.credentials.baseUrl && it.clientId == api.credentials.clientId }?.snapshot
    private val mutable = MutableStateFlow(RunnerCatalogState(initial))
    val state = mutable.asStateFlow()
    private var scope: CoroutineScope? = null
    private var worker: Job? = null
    private var generation = 0L
    private var first: CompletableDeferred<Unit>? = null
    private var persistedRevision = initial?.revision

    fun start(scope: CoroutineScope) {
        this.scope = scope
        if (worker?.isActive != true) restart(scope)
    }

    fun stop() {
        generation++
        worker?.cancel(); worker = null
        first?.cancel(); first = null
        mutable.update { it.copy(fresh = false) }
    }

    suspend fun refresh() {
        val active = scope
        if (active == null) {
            try {
                val snapshot = api.runnerCatalog(mutable.value.snapshot?.revision) ?: requireNotNull(mutable.value.snapshot)
                accept(snapshot, generation)
            } catch (e: Exception) {
                mutable.update { it.copy(fresh = false) }
                throw e
            }
        } else {
            // Restarting orders HTTP refreshes and SSE snapshots and discards old in-flight responses.
            if (first?.isCompleted != false) restart(active)
            first?.await()
        }
    }

    private fun restart(scope: CoroutineScope) {
        worker?.cancel()
        first?.cancel()
        val ready = CompletableDeferred<Unit>().also { first = it }
        val current = ++generation
        mutable.update { it.copy(fresh = false, error = null) }
        worker = scope.launch {
            try {
                var retry = 500L
                while (isActive && current == generation) {
                    try {
                        val snapshot = api.runnerCatalog(mutable.value.snapshot?.revision) ?: requireNotNull(mutable.value.snapshot)
                        accept(snapshot, current)
                        ready.complete(Unit)
                        api.runnerCatalogEvents(snapshot.revision).collect {
                            accept(it, current)
                            retry = 500L
                        }
                    } catch (e: CancellationException) { throw e }
                    catch (e: Exception) {
                        if (current == generation) mutable.update { it.copy(fresh = false, error = "Não foi possível atualizar os runners. Reconectando…") }
                        if (!ready.isCompleted) ready.completeExceptionally(e)
                        if (e is PoolApiException && e.status in listOf(401, 403)) break
                    }
                    if (current == generation) mutable.update { it.copy(fresh = false) }
                    delay(retry)
                    retry = minOf(retry * 2, 15_000L)
                }
            } finally {
                if (current == generation) mutable.update { it.copy(fresh = false) }
            }
        }
    }

    private suspend fun accept(snapshot: RunnerCatalogSnapshot, current: Long) {
        snapshot.validate()
        if (current != generation) return
        mutable.value = RunnerCatalogState(snapshot, fresh = true)
        if (persistedRevision != snapshot.revision) {
            try { persist(snapshot); persistedRevision = snapshot.revision }
            catch (e: CancellationException) { throw e }
            catch (e: Exception) { if (current == generation) mutable.update { it.copy(error = "O catálogo está atualizado, mas não foi possível salvar o cache.") } }
        }
    }
}

fun RunnerCatalogState.modelsForSession(session: Session): List<CatalogModel> {
    val capabilities = session.capabilities ?: return emptyList()
    val bound = runners.filter { it.id == session.runnerId }.map { it.copy(modelsKnown = true, models = capabilities.models) }
    return snapshot?.models.orEmpty().forRunners(bound, session.runnerId)
}
