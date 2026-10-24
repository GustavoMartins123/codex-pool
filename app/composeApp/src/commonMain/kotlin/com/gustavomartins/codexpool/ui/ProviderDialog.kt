package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import com.gustavomartins.codexpool.data.AppBuildConfig

@Composable
fun ProviderDialog(state: AuthState, select: (String) -> Unit, add: (String, String) -> Unit, dismiss: () -> Unit) {
    var name by remember { mutableStateOf("") }
    var url by remember { mutableStateOf("") }
    val scroll = rememberScrollState()
    LaunchedEffect(state.accounts?.selectedUrl) { name = ""; url = ""; scroll.scrollTo(0) }
    Dialog(onDismissRequest = { if (!state.loading) dismiss() }, properties = DialogProperties(usePlatformDefaultWidth = false)) {
        Surface(Modifier.padding(16.dp).widthIn(max = 560.dp).fillMaxWidth(), shape = MaterialTheme.shapes.large) {
            Column(Modifier.imePadding().verticalScroll(scroll).padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
                Text("Provedores", style = MaterialTheme.typography.titleLarge)
                Text("Cada domínio tem seu próprio login, sessões e runners.", color = MaterialTheme.colorScheme.onSurfaceVariant)
                state.accounts?.providers?.forEach { provider ->
                    OutlinedCard(onClick = { select(provider.baseUrl) }, enabled = !state.loading, modifier = Modifier.fillMaxWidth()) {
                        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                            Text(provider.name, style = MaterialTheme.typography.titleMedium)
                            Text(provider.baseUrl, style = MaterialTheme.typography.bodySmall)
                            Text(buildList {
                                if (provider.baseUrl == state.accounts.selectedUrl) add("Selecionado")
                                if (provider.baseUrl == AppBuildConfig.PoolUrl) add("Padrão")
                                add(if (provider.credentials != null) "Conectado" else "Login necessário")
                            }.joinToString(" · "), color = MaterialTheme.colorScheme.primary, style = MaterialTheme.typography.labelSmall)
                        }
                    }
                }
                HorizontalDivider()
                Text("Adicionar provedor", style = MaterialTheme.typography.titleMedium)
                OutlinedTextField(name, { name = it }, Modifier.fillMaxWidth(), singleLine = true,
                    enabled = !state.loading, label = { Text("Nome do provedor") })
                OutlinedTextField(url, { url = it }, Modifier.fillMaxWidth(), singleLine = true,
                    enabled = !state.loading, label = { Text("Domínio HTTPS") }, keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri))
                ErrorMessage(state.error)
                if (state.loading) LinearProgressIndicator(Modifier.fillMaxWidth())
                Button(onClick = { add(name, url) }, enabled = !state.loading && name.isNotBlank() && url.isNotBlank(), modifier = Modifier.fillMaxWidth()) {
                    Text("Adicionar e selecionar")
                }
                TextButton(onClick = dismiss, enabled = !state.loading, modifier = Modifier.fillMaxWidth()) { Text("Concluir") }
            }
        }
    }
}
