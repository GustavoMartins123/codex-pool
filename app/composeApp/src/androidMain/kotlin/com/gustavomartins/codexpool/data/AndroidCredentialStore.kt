package com.gustavomartins.codexpool.data

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.withContext

private val Context.credentialDataStore by preferencesDataStore("pool_credentials")

class AndroidCredentialStore(context: Context) : CredentialStore {
    private val store = context.applicationContext.credentialDataStore
    private val entry = stringPreferencesKey("encrypted_session")
    private val alias = "codex_pool_session_v1"

    private fun existingKey(): SecretKey {
        val keys = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        return keys.getKey(alias, null) as? SecretKey ?: error("A chave da credencial não está disponível. Saia para limpar a sessão.")
    }

    override suspend fun read(): PoolAccounts? = withContext(Dispatchers.IO) {
        val encoded = store.data.first()[entry] ?: return@withContext null
        val envelope = Base64.decode(encoded, Base64.NO_WRAP)
        require(envelope.size > 28) { "Credencial armazenada inválida." }
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.DECRYPT_MODE, existingKey(), GCMParameterSpec(128, envelope.copyOfRange(0, 12)))
        val decoded = decodeStoredAccounts(cipher.doFinal(envelope.copyOfRange(12, envelope.size)).decodeToString())
        if (decoded.migrated) write(decoded.accounts)
        decoded.accounts
    }

    override suspend fun write(accounts: PoolAccounts) = withContext(Dispatchers.IO) {
        accounts.validate()
        val keys = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        if (!keys.containsAlias(alias)) {
            KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore").apply {
                init(KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                    .setBlockModes(KeyProperties.BLOCK_MODE_GCM).setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                    .setKeySize(256).build())
                generateKey()
            }
        }
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.ENCRYPT_MODE, existingKey())
        val encrypted = cipher.doFinal(PoolJson.encodeToString(accounts).encodeToByteArray())
        store.edit { it[entry] = Base64.encodeToString(cipher.iv + encrypted, Base64.NO_WRAP) }
        Unit
    }

    override suspend fun clear() {
        store.edit { it.remove(entry) }
    }
}
