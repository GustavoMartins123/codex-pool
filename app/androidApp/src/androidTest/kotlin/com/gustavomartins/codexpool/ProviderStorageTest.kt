package com.gustavomartins.codexpool

import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import com.gustavomartins.codexpool.data.*
import kotlinx.coroutines.runBlocking
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class ProviderStorageTest {
    @Test fun encryptedAccountsSurviveStoreRecreationWithSeparateDomainCredentials() = runBlocking {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val store = AndroidCredentialStore(context)
        val first = "https://first.example"
        val second = "https://second.example"
        val accounts = PoolAccounts(selectedUrl = second, providers = listOf(
            PoolProvider("First", first, Credentials(first, "private-token-a", "client-a")),
            PoolProvider("Second", second, Credentials(second, "private-token-b", "client-b")),
        ))
        try {
            store.write(accounts)
            check(AndroidCredentialStore(context).read() == accounts)
            val bytes = context.filesDir.resolve("datastore/pool_credentials.preferences_pb").readBytes()
            check(!bytes.toString(Charsets.ISO_8859_1).contains("private-token"))
            check(!bytes.toString(Charsets.ISO_8859_1).contains("second.example"))
        } finally { store.clear() }
    }
}
