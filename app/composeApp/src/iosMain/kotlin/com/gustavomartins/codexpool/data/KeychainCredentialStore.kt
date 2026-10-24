@file:OptIn(kotlinx.cinterop.ExperimentalForeignApi::class)

package com.gustavomartins.codexpool.data

import kotlinx.cinterop.*
import platform.CoreFoundation.*
import platform.Security.*

class KeychainCredentialStore : CredentialStore {
    private inline fun <T> query(block: (CFMutableDictionaryRef) -> T): T {
        val query = checkNotNull(CFDictionaryCreateMutable(null, 0, null, null))
        val service = checkNotNull(CFStringCreateWithCString(null, "com.gustavomartins.codexpool.session", kCFStringEncodingUTF8))
        try {
            CFDictionarySetValue(query, kSecClass, kSecClassGenericPassword)
            CFDictionarySetValue(query, kSecAttrService, service)
            CFDictionarySetValue(query, kSecAttrAccount, service)
            return block(query)
        } finally {
            CFRelease(query)
            CFRelease(service)
        }
    }

    override suspend fun read(): PoolAccounts? {
        val decoded = readStored() ?: return null
        if (decoded.migrated) write(decoded.accounts)
        return decoded.accounts
    }

    private fun readStored(): DecodedAccounts? = query { query ->
        CFDictionarySetValue(query, kSecReturnData, kCFBooleanTrue)
        CFDictionarySetValue(query, kSecMatchLimit, kSecMatchLimitOne)
        memScoped {
            val result = alloc<CFTypeRefVar>()
            result.value = null
            val status = SecItemCopyMatching(query, result.ptr)
            if (status == errSecItemNotFound) return@memScoped null
            check(status == errSecSuccess) { "Falha ao ler o Keychain ($status)." }
            val raw = checkNotNull(result.value)
            try {
                check(CFGetTypeID(raw) == CFDataGetTypeID()) { "O Keychain retornou um tipo de credencial inválido." }
                val data: CFDataRef = raw.reinterpret()
                val bytes = checkNotNull(CFDataGetBytePtr(data)).readBytes(CFDataGetLength(data).toInt())
                decodeStoredAccounts(bytes.decodeToString())
            } finally { CFRelease(raw) }
        }
    }

    override suspend fun write(accounts: PoolAccounts) {
        val bytes = PoolJson.encodeToString(accounts.validate()).encodeToByteArray()
        bytes.usePinned { pinned ->
            val data = checkNotNull(CFDataCreate(null, pinned.addressOf(0).reinterpret(), bytes.size.toLong()))
            val attributes = checkNotNull(CFDictionaryCreateMutable(null, 0, null, null))
            try {
                CFDictionarySetValue(attributes, kSecValueData, data)
                CFDictionarySetValue(attributes, kSecAttrAccessible, kSecAttrAccessibleWhenUnlockedThisDeviceOnly)
                query { query ->
                    val updated = SecItemUpdate(query, attributes)
                    if (updated == errSecItemNotFound) {
                        CFDictionarySetValue(query, kSecValueData, data)
                        CFDictionarySetValue(query, kSecAttrAccessible, kSecAttrAccessibleWhenUnlockedThisDeviceOnly)
                        val added = SecItemAdd(query, null)
                        check(added == errSecSuccess) { "Falha ao gravar o Keychain ($added)." }
                    } else check(updated == errSecSuccess) { "Falha ao atualizar o Keychain ($updated)." }
                }
            } finally { CFRelease(attributes); CFRelease(data) }
        }
    }

    override suspend fun clear() {
        query { query ->
            val status = SecItemDelete(query)
            check(status == errSecSuccess || status == errSecItemNotFound) { "Falha ao limpar o Keychain ($status)." }
        }
    }
}
