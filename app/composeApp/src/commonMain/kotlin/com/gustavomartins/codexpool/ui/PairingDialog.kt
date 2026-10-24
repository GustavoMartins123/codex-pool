package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.gustavomartins.codexpool.data.Decision

@Composable
fun PairingDialog(state: DashboardState, vm: DashboardViewModel, onScan: (() -> Unit)?) {
    var link by rememberSaveable { mutableStateOf("") }
    var code by rememberSaveable(state.pairing?.id) { mutableStateOf("") }
    AlertDialog(onDismissRequest = vm::dismissPairing,
        title = { Text(if (state.pairingComplete) "Decisão enviada" else "Conectar máquina") },
        text = {
            Column(Modifier.heightIn(max = 480.dp).verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                if (state.pairingComplete) {
                    Text("O computador receberá a decisão. A credencial fica somente na máquina.")
                } else if (state.pairing == null) {
                    Text("Inicie runnerd pair --url ${com.gustavomartins.codexpool.data.AppBuildConfig.PoolUrl} --name minha-maquina no computador.")
                    Text("Leia o QR ou abra o link gerado por ele. Só autorize máquinas que você reconhece.")
                    if (onScan != null) OutlinedButton(onClick = onScan, enabled = !state.loading) { Text("Ler QR Code") }
                    OutlinedTextField(link, { link = it }, label = { Text("Link de pareamento") }, enabled = !state.loading)
                    Button(onClick = { vm.reviewPairing(link.trim()) }, enabled = !state.loading && link.isNotBlank()) { Text("Verificar máquina") }
                } else {
                    val machine = state.pairing
                    Text(machine.name, style = MaterialTheme.typography.titleLarge)
                    Text("${machine.os} · ${machine.arch}")
                    Text(if (machine.runnerId == null) "Esta máquina poderá executar sessões da sua conta." else "A credencial desta máquina será substituída.")
                    Text("Confira a máquina e digite o código exibido ao lado do QR no computador.")
                    OutlinedTextField(code, { code = it }, label = { Text("Código no computador") }, singleLine = true, enabled = !state.loading)
                    Text("Expira em ${machine.expiresAt}", style = MaterialTheme.typography.bodySmall)
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedButton(onClick = { vm.decidePairing(machine.id, Decision.Deny, "") }, enabled = !state.loading) { Text("Negar") }
                        Button(onClick = { vm.decidePairing(machine.id, Decision.Approve, code.trim()) }, enabled = !state.loading && code.trim().length == 8) { Text("Autorizar máquina") }
                    }
                }
                ErrorMessage(state.error)
                if (state.loading) LinearProgressIndicator(Modifier.fillMaxWidth())
            }
        },
        confirmButton = { TextButton(onClick = vm::dismissPairing, enabled = !state.loading) { Text("Concluir") } })
}
