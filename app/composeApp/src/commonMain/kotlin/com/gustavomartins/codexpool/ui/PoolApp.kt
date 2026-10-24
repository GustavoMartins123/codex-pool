package com.gustavomartins.codexpool.ui

import androidx.compose.foundation.Image
import androidx.compose.foundation.layout.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ColorFilter
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModelStore
import androidx.lifecycle.ViewModelStoreOwner
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.LocalViewModelStoreOwner
import androidx.navigation.compose.*
import androidx.navigation.toRoute
import codex_pool_app.composeapp.generated.resources.*
import com.gustavomartins.codexpool.data.*
import io.ktor.client.HttpClient
import kotlinx.serialization.Serializable
import org.jetbrains.compose.resources.Font
import org.jetbrains.compose.resources.painterResource
import org.koin.compose.KoinApplication
import org.koin.compose.viewmodel.koinViewModel
import org.koin.core.module.dsl.viewModel
import org.koin.dsl.module
import org.koin.dsl.koinConfiguration
import org.koin.core.parameter.parametersOf

private val Gold = Color(0xFFD5A638)
private val PoolColors = darkColorScheme(
    primary = Gold, onPrimary = Color(0xFF201A0C), secondary = Color(0xFFB9AA88),
    background = Color(0xFF0D0E0D), onBackground = Color(0xFFEAE6DD),
    surface = Color(0xFF171A18), onSurface = Color(0xFFEAE6DD),
    surfaceVariant = Color(0xFF252B25), onSurfaceVariant = Color(0xFFBABFB2),
    outline = Color(0xFF62695D), error = Color(0xFFFFB4AB),
)

@Serializable private data object DashboardRoute
@Serializable private data object NewSessionRoute
@Serializable private data class SessionRoute(val id: String)

@Composable
fun PoolApp(store: CredentialStore, client: HttpClient) {
    val dependencies = remember(store, client) {
        module {
            single<CredentialStore> { store }
            single { client }
            viewModel { AuthViewModel(get(), get(), AppBuildConfig.PoolUrl) }
            viewModel { parameters -> DashboardViewModel(parameters.get()) }
            viewModel { parameters -> SessionViewModel(parameters.get(), parameters.get()) }
        }
    }
    val typography = Typography(
        headlineLarge = MaterialTheme.typography.headlineLarge.copy(fontFamily = FontFamily(Font(Res.font.cormorant_500))),
        titleLarge = MaterialTheme.typography.titleLarge.copy(fontFamily = FontFamily(Font(Res.font.plexsanscondensed_500))),
        bodyLarge = MaterialTheme.typography.bodyLarge.copy(fontFamily = FontFamily(Font(Res.font.plexsanscondensed_400))),
        labelSmall = MaterialTheme.typography.labelSmall.copy(fontFamily = FontFamily(Font(Res.font.plexmono_400))),
    )
    val configuration = remember(dependencies) { koinConfiguration { modules(dependencies) } }
    KoinApplication(configuration) {
        MaterialTheme(colorScheme = PoolColors, typography = typography) {
            Surface(Modifier.fillMaxSize()) {
                val auth = koinViewModel<AuthViewModel>()
                val state by auth.state.collectAsStateWithLifecycle()
                val credentials = state.credentials
                if (credentials == null) {
                    LoginScreen(state, auth::login, auth::logout)
                } else key(credentials) {
                    val viewModels = remember { ViewModelStore() }
                    val owner = remember { object : ViewModelStoreOwner { override val viewModelStore = viewModels } }
                    DisposableEffect(viewModels) { onDispose { viewModels.clear() } }
                    CompositionLocalProvider(LocalViewModelStoreOwner provides owner) {
                        val api = remember { PoolApi(client, credentials) }
                        AuthenticatedApp(api, state, auth::logout)
                    }
                }
            }
        }
    }
}

@Composable
private fun AuthenticatedApp(api: PoolApi, auth: AuthState, logout: () -> Unit) {
    val nav = rememberNavController()
    val dashboard = koinViewModel<DashboardViewModel>(parameters = { parametersOf(api) })
    val state by dashboard.state.collectAsStateWithLifecycle()
    NavHost(navController = nav, startDestination = DashboardRoute) {
        composable<DashboardRoute> {
            DashboardScreen(state, api.credentials.baseUrl, auth, dashboard, logout,
                onCreate = { dashboard.loadCatalog(); nav.navigate(NewSessionRoute) },
                onOpen = { nav.navigate(SessionRoute(it)) })
        }
        composable<NewSessionRoute> {
            CreateSessionScreen(state, dashboard::loadCatalog,
                onBack = { nav.popBackStack() }, onCreate = { request ->
                    dashboard.create(request) { id ->
                        nav.navigate(SessionRoute(id)) { popUpTo<DashboardRoute>() }
                    }
                })
        }
        composable<SessionRoute> { entry ->
            val id = entry.toRoute<SessionRoute>().id
            val session = koinViewModel<SessionViewModel>(viewModelStoreOwner = entry, parameters = { parametersOf(api, id) })
            SessionScreen(session, onBack = { nav.popBackStack(); dashboard.refresh() })
        }
    }
}

@Composable
fun Brand() {
    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        Image(painterResource(Res.drawable.crest_mask), contentDescription = null,
            modifier = Modifier.size(38.dp), colorFilter = ColorFilter.tint(Gold))
        Column {
            Text("codex-pool", style = MaterialTheme.typography.titleLarge)
            Text("AGENTES · EXECUÇÃO REMOTA", color = MaterialTheme.colorScheme.primary, style = MaterialTheme.typography.labelSmall)
        }
    }
}

@Composable
fun ErrorMessage(message: String?) {
    if (message != null) Surface(color = MaterialTheme.colorScheme.errorContainer, shape = MaterialTheme.shapes.medium) {
        Text(message, Modifier.fillMaxWidth().padding(16.dp), color = MaterialTheme.colorScheme.onErrorContainer)
    }
}

fun SessionState.label(): String = when (this) {
    SessionState.Created -> "Criada"
    SessionState.PendingRunner -> "Aguardando runner"
    SessionState.Claimed -> "Preparando workspace"
    SessionState.Running -> "Em execução"
    SessionState.WaitingInput -> "Aguardando mensagem"
    SessionState.WaitingApproval -> "Aprovação pendente"
    SessionState.Orphaned -> "Runner desconectado"
    SessionState.Closing -> "Encerrando"
    SessionState.Closed -> "Encerrada"
}
