package com.gustavomartins.codexpool.data

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlin.time.Duration
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.seconds

enum class BootstrapStep { Connecting, Verifying, Confirm, Authorizing, Saving, WaitingOnline, Online, Failed, Cancelled }

data class BootstrapProgress(
    val step: BootstrapStep,
    val ticket: BootstrapTicket? = null,
    val runnerId: String? = null,
    val message: String? = null,
)

/**
 * Onboards a runner from its LAN QR for the account captured at scan time:
 * [pool] keeps that account's credentials and [rememberKey] stores the runner's
 * command key with that account, whatever is selected later.
 */
class BootstrapCoordinator(
    private val pool: PoolApi,
    private val connectLocal: suspend () -> BootstrapApi,
    private val rememberKey: suspend (String) -> Unit,
    private val pause: suspend (Duration) -> Unit = { delay(it) },
    private val operationId: String = newBootstrapOperationId(),
) {
    private val mutable = MutableStateFlow(BootstrapProgress(BootstrapStep.Connecting))
    val progress = mutable.asStateFlow()
    private var local: BootstrapApi? = null
    private var approvalSent = false

    /** Hands the runner this account's pool and checks the pool shows the same machine. */
    suspend fun prepare() = failing {
        val api = connectLocal().also { local = it }
        val origin = normalizeOrigin(pool.credentials.baseUrl)
        val ticket = api.begin(operationId, origin)
        require(ticket.operationId == operationId && ticket.poolUrl == origin && ticket.userCode.matches(Regex("[A-Za-z0-9_-]{8}")) &&
            CommandSigning.keyId(CommandSigning.decodeKey(ticket.commandKey)) == ticket.commandKeyId) {
            "O computador respondeu com outro pareamento. Nada foi autorizado."
        }
        mutable.value = BootstrapProgress(BootstrapStep.Verifying, ticket)
        val review = pool.reviewPairingRequest(ticket.pairingId)
        require(review.name == ticket.name && review.os == ticket.os && review.arch == ticket.arch && review.runnerId == null) {
            "O pedido no pool não corresponde à máquina do QR. Nada foi autorizado."
        }
        mutable.value = BootstrapProgress(BootstrapStep.Confirm, ticket)
    }

    /** Saves the key before approving: a runner whose key was lost refuses every command. */
    suspend fun approve() = failing {
        val ticket = requireNotNull(mutable.value.ticket?.takeIf { mutable.value.step == BootstrapStep.Confirm }) { "Nenhuma máquina aguardando autorização." }
        mutable.value = BootstrapProgress(BootstrapStep.Authorizing, ticket)
        rememberKey(ticket.commandKey)
        approvalSent = true
        pool.decidePairing(ticket.pairingId, Decision.Approve, ticket.userCode)
        mutable.value = BootstrapProgress(BootstrapStep.Saving, ticket)
        val runnerId = awaitSaved(ticket)
        mutable.value = BootstrapProgress(BootstrapStep.WaitingOnline, ticket, runnerId)
        if (awaitOnline(ticket, runnerId)) mutable.value = BootstrapProgress(BootstrapStep.Online, ticket, runnerId)
        else mutable.value = BootstrapProgress(BootstrapStep.WaitingOnline, ticket, runnerId,
            "Autorizado. O runner ainda não ficou online: confira no computador se Codex ou Claude Code está instalado.")
    }

    suspend fun deny() = failing {
        val ticket = requireNotNull(mutable.value.ticket) { "Nenhuma máquina aguardando autorização." }
        pool.decidePairing(ticket.pairingId, Decision.Deny, "")
        runCatching { local?.cancel(operationId) }
        mutable.value = BootstrapProgress(BootstrapStep.Cancelled, ticket)
    }

    /** Before approval the runner forgets the attempt; after it, only this screen stops. */
    suspend fun cancel() {
        if (!approvalSent) runCatching { local?.cancel(operationId) }
        close()
    }

    fun close() { local?.close(); local = null }

    // The runner reports the saved credential over the LAN; if the phone lost
    // the LAN after approving, the pool's runner list confirms it instead.
    private suspend fun awaitSaved(ticket: BootstrapTicket): String? {
        var misses = 0
        repeat(300) {
            val status = runCatching { local?.status() }.getOrNull()
            if (status == null) {
                if (++misses >= 5) return null
            } else {
                misses = 0
                when (status.phase) {
                    BootstrapPhase.Paired -> {
                        check(status.commandKeyId == ticket.commandKeyId) { "O computador salvou outra chave. Revogue esse runner." }
                        runCatching { local?.ack(operationId) }
                        return status.runnerId
                    }
                    BootstrapPhase.Failed -> throw BootstrapFailure(status.error, bootstrapMessage(status.error) ?: status.message ?: "O pareamento falhou.")
                    BootstrapPhase.Cancelled, BootstrapPhase.Expired -> error("O computador encerrou o pareamento.")
                    else -> Unit
                }
            }
            pause(1.seconds)
        }
        return null
    }

    private suspend fun awaitOnline(ticket: BootstrapTicket, runnerId: String?): Boolean {
        val attempts = (2.minutes / 2.seconds).toInt()
        repeat(attempts) {
            val runners = runCatching { pool.runners() }.getOrNull().orEmpty()
            val runner = if (runnerId != null) runners.firstOrNull { it.id == runnerId } else runners.firstOrNull { it.name == ticket.name && it.online && !it.revoked }
            if (runner?.online == true && !runner.revoked) return true
            pause(2.seconds)
        }
        return false
    }

    private suspend fun failing(block: suspend () -> Unit) {
        try { block() }
        catch (e: CancellationException) { throw e }
        catch (e: Exception) {
            mutable.value = mutable.value.copy(step = BootstrapStep.Failed, message = e.message ?: "O pareamento falhou.")
            throw e
        }
    }
}
