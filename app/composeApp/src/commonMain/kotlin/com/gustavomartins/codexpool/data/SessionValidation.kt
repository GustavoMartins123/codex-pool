package com.gustavomartins.codexpool.data

fun CreateSession.validate() {
    require(model.isNotBlank()) { "Selecione um modelo do catálogo." }
    when (workspace.mode) {
        WorkspaceMode.Ephemeral -> require(workspace.target == null) { "Workspace efêmero não recebe destino." }
        WorkspaceMode.Path -> {
            require(!workspace.target.isNullOrBlank()) { "Informe o caminho permitido pelo runner." }
            require(permissionMode == PermissionMode.Gated) { "Workspace por caminho exige aprovação de comandos." }
        }
        WorkspaceMode.Repo -> require(!workspace.target.isNullOrBlank()) { "Informe a URL do repositório." }
    }
}
