@file:OptIn(androidx.compose.material3.ExperimentalMaterial3Api::class)

package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import com.gustavomartins.codexpool.data.*

@Composable
fun LoginScreen(state: AuthState, login: (String, String) -> Unit, clear: () -> Unit, onProviders: () -> Unit) {
    var username by rememberSaveable { mutableStateOf("") }
    // Passwords are never saved into activity or navigation state.
    var password by remember { mutableStateOf("") }
    Column(Modifier.fillMaxSize().safeDrawingPadding().imePadding().verticalScroll(rememberScrollState()).padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(20.dp)) {
        Spacer(Modifier.height(24.dp))
        Brand()
        Spacer(Modifier.height(28.dp))
        Text("Seu código.\nSuas máquinas.", style = MaterialTheme.typography.headlineLarge)
        Text("Abra sessões de Codex e Claude nos runners conectados à sua conta.", color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text("Entre com a mesma conta do site.", color = MaterialTheme.colorScheme.onSurfaceVariant)
        TextButton(onClick = onProviders, enabled = !state.loading && state.accounts != null) {
            Text("Provedor: ${state.accounts?.selected?.name.orEmpty()}")
        }
        OutlinedTextField(username, { username = it }, Modifier.fillMaxWidth(), label = { Text("Usuário ou e-mail") }, singleLine = true, enabled = !state.loading)
        OutlinedTextField(password, { password = it }, Modifier.fillMaxWidth(), label = { Text("Senha") }, singleLine = true,
            visualTransformation = PasswordVisualTransformation(), keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password), enabled = !state.loading)
        ErrorMessage(state.error)
        Button(onClick = { login(username, password); password = "" }, Modifier.fillMaxWidth().height(52.dp),
            enabled = !state.loading && state.accounts != null && username.isNotBlank() && password.isNotEmpty()) { Text("Entrar no pool") }
        if (state.loading) LinearProgressIndicator(Modifier.fillMaxWidth())
        if (state.error != null && state.accounts == null) TextButton(onClick = clear, enabled = !state.loading) { Text("Redefinir dados locais") }
    }
}

@Composable
fun DashboardScreen(state: DashboardState, auth: AuthState, vm: DashboardViewModel,
    logout: () -> Unit, onScanPairing: (() -> Unit)?, onProviders: () -> Unit, onCreate: () -> Unit, onOpen: (String) -> Unit) {
    var revoke by remember { mutableStateOf<Runner?>(null) }
    var menuOpen by remember { mutableStateOf(false) }
    Scaffold(
        topBar = { TopAppBar(title = { Brand() }, actions = {
            TextButton(onClick = vm::refresh, enabled = !state.loading) { Text("Atualizar") }
            Box {
                TextButton(onClick = { menuOpen = true }, enabled = !auth.loading) { Text("Conta") }
                DropdownMenu(expanded = menuOpen, onDismissRequest = { menuOpen = false }) {
                    DropdownMenuItem(text = { Text("Provedores") }, onClick = { menuOpen = false; onProviders() })
                    DropdownMenuItem(text = { Text("Sair") }, onClick = { menuOpen = false; logout() })
                }
            }
        }) },
        bottomBar = {
            NavigationBar {
                DashboardTab.entries.forEach { tab ->
                    val title = when (tab) { DashboardTab.Sessions -> "Sessões"; DashboardTab.Runners -> "Runners"; DashboardTab.Quotas -> "Quotas" }
                    NavigationBarItem(selected = state.tab == tab, enabled = !state.loading, onClick = { vm.select(tab) },
                        icon = { Text(when (tab) { DashboardTab.Sessions -> ">_"; DashboardTab.Runners -> "▣"; DashboardTab.Quotas -> "%" }) }, label = { Text(title) })
                }
            }
        },
        floatingActionButton = {
            if (state.tab == DashboardTab.Sessions) ExtendedFloatingActionButton(onClick = onCreate) { Text("+ Nova sessão") }
            if (state.tab == DashboardTab.Runners) ExtendedFloatingActionButton(onClick = vm::openPairing, expanded = true, text = { Text("Conectar runner") }, icon = { Text("+") })
        },
    ) { padding ->
        LazyColumn(Modifier.fillMaxSize().padding(padding).consumeWindowInsets(padding), contentPadding = PaddingValues(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
            item { Text(auth.accounts?.selected?.name.orEmpty(), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
            item { Text(when (state.tab) { DashboardTab.Sessions -> "Suas sessões"; DashboardTab.Runners -> "Suas máquinas"; DashboardTab.Quotas -> "Uso disponível" }, style = MaterialTheme.typography.headlineLarge) }
            if (state.loading) item { LinearProgressIndicator(Modifier.fillMaxWidth()) }
            item { ErrorMessage(state.error); ErrorMessage(auth.error) }
            when (state.tab) {
                DashboardTab.Sessions -> {
                    item {
                        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                            ConversationStorage.entries.forEach { storage ->
                                FilterChip(selected = state.storage == storage, enabled = !state.loading,
                                    onClick = { vm.selectStorage(storage) }, label = { Text(if (storage == ConversationStorage.Active) "Ativas" else "Arquivadas") })
                            }
                        }
                    }
                    if (state.conversations?.isEmpty() == true) item { EmptyCard(
                        if (state.storage == ConversationStorage.Active) "Nenhuma sessão" else "Nenhuma conversa arquivada",
                        if (state.storage == ConversationStorage.Active) "Conecte um runner e abra sua primeira sessão de código." else "Conversas inativas serão guardadas aqui com o histórico completo.") }
                    items(state.conversations.orEmpty(), key = { it.id }) { conversation ->
                        Card(onClick = { onOpen(conversation.id) }, modifier = Modifier.fillMaxWidth()) {
                            Column(Modifier.padding(18.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                                Text("${conversation.agent.name} · ${conversation.model}", style = MaterialTheme.typography.titleLarge)
                                if (conversation.title.isNotBlank()) Text(conversation.title, style = MaterialTheme.typography.titleMedium)
                                Text(conversation.sessionState.label(), color = MaterialTheme.colorScheme.primary)
                                Text(conversation.id, style = MaterialTheme.typography.labelSmall)
                                conversation.lastInteractionAt?.let { Text(it, style = MaterialTheme.typography.labelSmall) }
                                conversation.preview.forEach { message ->
                                    Text(if (message.role == "user") "Você" else "Agente", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.primary)
                                    Text(message.text, maxLines = 2, overflow = androidx.compose.ui.text.style.TextOverflow.Ellipsis,
                                        style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                                }
                            }
                        }
                    }
                    if (state.nextCursor != null) item {
                        OutlinedButton(onClick = vm::loadMoreConversations, enabled = !state.loading, modifier = Modifier.fillMaxWidth()) { Text("Carregar mais conversas") }
                    }
                }
                DashboardTab.Runners -> {
                    if (state.runners?.isEmpty() == true) item { EmptyCard("Nenhum runner conectado", "Gere um nonce de registro e use-o na máquina que executará os agentes.") }
                    items(state.runners.orEmpty(), key = { it.id }) { runner ->
                        Card(Modifier.fillMaxWidth()) {
                            Column(Modifier.padding(18.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                                Text(runner.name, style = MaterialTheme.typography.titleLarge)
                                Text(if (runner.revoked) "Revogado" else if (runner.online) "Online" else "Offline", color = if (runner.online) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant)
                                Text(runner.agents.joinToString { it.name })
                                if (runner.labels.isNotEmpty()) Text(runner.labels.joinToString(" · "))
                                if (runner.allowedPaths.isNotEmpty()) SelectionContainer { Text(runner.allowedPaths.joinToString("\n"), style = MaterialTheme.typography.labelSmall) }
                                if (!runner.revoked) {
                                    TextButton(onClick = { revoke = runner }, enabled = !state.loading) { Text("Revogar runner", color = MaterialTheme.colorScheme.error) }

                                }
                            }
                        }
                    }
                }
                DashboardTab.Quotas -> {
                    state.quotas?.let { quotas ->
                        item { Text("Atualizado: ${quotas.generatedAt}", style = MaterialTheme.typography.labelSmall) }
                        if (quotas.models.isEmpty()) item { EmptyCard("Sem quotas disponíveis", "O pool não retornou modelos para sua conta.") }
                        items(quotas.models, key = { "${it.provider}/${it.model}" }) { quota ->
                            Card(Modifier.fillMaxWidth()) {
                                Column(Modifier.padding(18.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                                    Text(quota.name, style = MaterialTheme.typography.titleLarge)
                                    Text(quota.provider, color = MaterialTheme.colorScheme.onSurfaceVariant)
                                    Text(if (quota.availableNow) "Disponível agora" else "Indisponível agora")
                                    QuotaLine("Janela primária", quota.primary)
                                    QuotaLine("Janela secundária", quota.secondary)
                                }
                            }
                        }
                    }
                }
            }
            item { Spacer(Modifier.height(80.dp)) }
        }
    }
    val runner = revoke
    if (runner != null) AlertDialog(onDismissRequest = { revoke = null }, title = { Text("Revogar ${runner.name}?") },
        text = { Text("Essa máquina perderá a autorização para executar sessões.") },
        confirmButton = { TextButton(onClick = { vm.revoke(runner.id); revoke = null }) { Text("Revogar") } },
        dismissButton = { TextButton(onClick = { revoke = null }) { Text("Voltar") } })
    if (state.pairingOpen) PairingDialog(state, vm, onScanPairing)

}

@Composable
private fun QuotaLine(label: String, window: QuotaWindow?) {
    if (window == null) Text("$label: não informada", color = MaterialTheme.colorScheme.onSurfaceVariant)
    else {
        Text("$label: ${window.remainingPercent.toInt()}% restante")
        LinearProgressIndicator(progress = { (window.remainingPercent / 100).toFloat() }, modifier = Modifier.fillMaxWidth())
    }
}

@Composable
private fun EmptyCard(title: String, message: String) {
    OutlinedCard(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(24.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Text(title, style = MaterialTheme.typography.titleLarge)
            Text(message, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
fun CreateSessionScreen(state: DashboardState, reloadCatalog: () -> Unit, onBack: () -> Unit, onCreate: (CreateSession) -> Unit) {
    var agent by rememberSaveable { mutableStateOf(Agent.Codex) }
    var model by rememberSaveable { mutableStateOf("") }
    var effort by rememberSaveable { mutableStateOf("") }
    var mode by rememberSaveable { mutableStateOf(WorkspaceMode.Ephemeral) }
    var target by rememberSaveable { mutableStateOf("") }
    var permission by rememberSaveable { mutableStateOf(PermissionMode.Gated) }
    var prompt by rememberSaveable { mutableStateOf("") }
    Scaffold(topBar = { TopAppBar(title = { Text("Nova sessão") }, navigationIcon = { TextButton(onClick = onBack) { Text("Voltar") } }) }) { padding ->
        Column(Modifier.fillMaxSize().padding(padding).imePadding().verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
            Text("Escolha como trabalhar", style = MaterialTheme.typography.headlineLarge)
            Text("Agente")
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                Agent.entries.forEach { item -> FilterChip(selected = agent == item, enabled = !state.loading, onClick = { agent = item; model = ""; effort = "" }, label = { Text(item.name) }) }
            }
            ModelSettingsFields(agent, state.catalog.orEmpty(), TurnSettings(model, effort), !state.loading) { model = it.model; effort = it.reasoningEffort }
            if (state.catalog?.isEmpty() == true) Text("O pool não retornou modelos.")
            if (state.catalog == null && !state.loading) TextButton(onClick = reloadCatalog) { Text("Carregar catálogo") }
            Text("Workspace")
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                WorkspaceMode.entries.forEach { item ->
                    FilterChip(selected = mode == item, enabled = !state.loading, onClick = { mode = item; if (item == WorkspaceMode.Path) permission = PermissionMode.Gated },
                        label = { Text(when (item) { WorkspaceMode.Ephemeral -> "Efêmero"; WorkspaceMode.Path -> "Caminho"; WorkspaceMode.Repo -> "Repositório" }) })
                }
            }
            if (mode != WorkspaceMode.Ephemeral) OutlinedTextField(target, { target = it }, Modifier.fillMaxWidth(), enabled = !state.loading,
                label = { Text(if (mode == WorkspaceMode.Path) "Caminho na allowlist do runner" else "URL do repositório") }, singleLine = true)
            Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text("Aprovar comandos", style = MaterialTheme.typography.titleMedium)
                    Text(if (mode == WorkspaceMode.Path) "Obrigatório para workspace por caminho" else "O agente aguarda sua decisão", style = MaterialTheme.typography.bodySmall)
                }
                Switch(checked = permission == PermissionMode.Gated, enabled = mode != WorkspaceMode.Path && !state.loading,
                    onCheckedChange = { permission = if (it) PermissionMode.Gated else PermissionMode.Auto })
            }
            OutlinedTextField(prompt, { prompt = it }, Modifier.fillMaxWidth().heightIn(min = 130.dp), label = { Text("Primeira mensagem (opcional)") }, enabled = !state.loading)
            ErrorMessage(state.error)
            if (state.loading) LinearProgressIndicator(Modifier.fillMaxWidth())
            Button(onClick = { onCreate(CreateSession(agent, model, Workspace(mode, if (mode == WorkspaceMode.Ephemeral) null else target.trim()), permission, prompt.takeIf { it.isNotBlank() }, effort)) },
                enabled = !state.loading && state.catalog?.any { it.id == model && it.availableNow && it.supports(agent) } == true && (mode == WorkspaceMode.Ephemeral || target.isNotBlank()), modifier = Modifier.fillMaxWidth().height(52.dp)) { Text("Abrir sessão") }
        }
    }
}
