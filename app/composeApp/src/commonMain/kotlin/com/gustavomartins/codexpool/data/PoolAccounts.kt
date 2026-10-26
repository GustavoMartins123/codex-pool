package com.gustavomartins.codexpool.data

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.jsonObject

@Serializable
data class PoolProvider(val name: String, val baseUrl: String, val credentials: Credentials? = null,
    val commandKeys: List<String> = emptyList())

@Serializable
data class PoolAccounts(val version: Int = 2, val selectedUrl: String, val providers: List<PoolProvider>) {
    val selected get() = providers.single { it.baseUrl == selectedUrl }

    fun validate(): PoolAccounts {
        require(version == 2) { "Versão de armazenamento de provedores incompatível." }
        require(providers.isNotEmpty() && providers.map { it.baseUrl }.distinct().size == providers.size) { "Lista de provedores inválida." }
        providers.forEach {
            require(it.name.isNotBlank() && it.name.length <= 60 && normalizePoolUrl(it.baseUrl) == it.baseUrl) { "Provedor armazenado inválido." }
            it.credentials?.let { credentials ->
                require(credentials.baseUrl == it.baseUrl && credentials.token.isNotBlank() && credentials.clientId.isNotBlank()) {
                    "A credencial não pertence ao domínio do provedor."
                }
            }
            require(it.commandKeys.size <= RunnerContract.MaxCommandSignatures && it.commandKeys.distinct().size == it.commandKeys.size) {
                "Chaves de runner armazenadas inválidas."
            }
            it.commandKeys.forEach(CommandSigning::decodeKey)
        }
        require(providers.count { it.baseUrl == selectedUrl } == 1) { "O provedor selecionado não está cadastrado." }
        return this
    }

    fun withCredentials(credentials: Credentials?): PoolAccounts = copy(providers = providers.map {
        if (it.baseUrl == selectedUrl) it.copy(credentials = credentials) else it
    }).validate()

    fun withCommandKey(key: String): PoolAccounts {
        require(key in selected.commandKeys || selected.commandKeys.size < RunnerContract.MaxCommandSignatures) {
            "Limite de ${RunnerContract.MaxCommandSignatures} chaves de runner atingido neste provedor."
        }
        return copy(providers = providers.map {
            if (it.baseUrl == selectedUrl) it.copy(commandKeys = it.commandKeys - key + key) else it
        }).validate()
    }

    companion object {
        fun initial(defaultUrl: String): PoolAccounts {
            val url = normalizePoolUrl(defaultUrl)
            return PoolAccounts(selectedUrl = url, providers = listOf(PoolProvider("Principal", url)))
        }
    }
}

data class DecodedAccounts(val accounts: PoolAccounts, val migrated: Boolean)

fun decodeStoredAccounts(raw: String): DecodedAccounts {
    val objectValue = PoolJson.parseToJsonElement(raw).jsonObject
    // One-time conversion of the single-credential schema, persisted before it can be used.
    if ("version" !in objectValue && "baseUrl" in objectValue) {
        val credentials = PoolJson.decodeFromString<Credentials>(raw)
        val accounts = PoolAccounts(selectedUrl = credentials.baseUrl,
            providers = listOf(PoolProvider("Principal", credentials.baseUrl, credentials))).validate()
        return DecodedAccounts(accounts, true)
    }
    require("version" in objectValue) { "Armazenamento de provedores sem versão." }
    return DecodedAccounts(PoolJson.decodeFromString<PoolAccounts>(raw).validate(), false)
}
