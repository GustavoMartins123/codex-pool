@file:OptIn(androidx.compose.material3.ExperimentalMaterial3Api::class)

package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.layout.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import com.gustavomartins.codexpool.data.*

fun effortLabel(value: String): String = when (value) {
    "" -> "Padrão do provedor"
    "none" -> "Sem raciocínio"
    "minimal" -> "Mínimo"
    "low" -> "Baixo"
    "medium" -> "Médio"
    "high" -> "Alto"
    "xhigh" -> "Muito alto"
    "max" -> "Máximo"
    else -> error("Esforço de raciocínio desconhecido: $value")
}

@Composable
fun ModelSettingsFields(agent: Agent, catalog: List<CatalogModel>, settings: TurnSettings,
    enabled: Boolean, onSelect: (TurnSettings) -> Unit) {
    var modelMenu by remember { mutableStateOf(false) }
    var effortMenu by remember { mutableStateOf(false) }
    val models = catalog.filter { it.supports(agent) }
    val selected = models.singleOrNull { it.id == settings.model }
    ExposedDropdownMenuBox(expanded = modelMenu, onExpandedChange = { if (enabled) modelMenu = it }) {
        OutlinedTextField(settings.model, {}, readOnly = true, enabled = enabled,
            label = { Text("Modelo do pool") }, singleLine = true,
            modifier = Modifier.fillMaxWidth().testTag("model-selector").menuAnchor(ExposedDropdownMenuAnchorType.PrimaryNotEditable),
            trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(modelMenu) })
        ExposedDropdownMenu(expanded = modelMenu, onDismissRequest = { modelMenu = false }) {
            models.forEach { model ->
                DropdownMenuItem(enabled = model.availableNow,
                    text = { Column { Text(model.name ?: model.id); Text(model.id + if (model.availableNow) "" else " · indisponível", style = MaterialTheme.typography.bodySmall) } },
                    onClick = { onSelect(TurnSettings(model.id, model.defaultReasoningEffort)); modelMenu = false })
            }
        }
    }
    val efforts = selected?.efforts(agent).orEmpty()
    if (efforts.isNotEmpty()) {
        ExposedDropdownMenuBox(expanded = effortMenu, onExpandedChange = { if (enabled) effortMenu = it }) {
            OutlinedTextField(effortLabel(settings.reasoningEffort), {}, readOnly = true, enabled = enabled,
                label = { Text("Esforço de raciocínio") }, singleLine = true,
                modifier = Modifier.fillMaxWidth().testTag("effort-selector").menuAnchor(ExposedDropdownMenuAnchorType.PrimaryNotEditable),
                trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(effortMenu) })
            ExposedDropdownMenu(expanded = effortMenu, onDismissRequest = { effortMenu = false }) {
                (listOf("") + efforts).forEach { effort ->
                    DropdownMenuItem(text = { Text(effortLabel(effort)) }, onClick = { onSelect(settings.copy(reasoningEffort = effort)); effortMenu = false })
                }
            }
        }
    } else if (selected != null) {
        Text("O catálogo não oferece ajuste de raciocínio para este modelo.", style = MaterialTheme.typography.bodySmall)
    }
    if (models.isEmpty()) Text("Nenhum modelo compatível com ${agent.name} neste pool.")
}
