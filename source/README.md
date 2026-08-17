# Discord Go Live Launcher — Go v3

Utilitário standalone em Go para macOS e Windows. Não exige Node, Python, Vencord ou alteração do proxy global do sistema.

## Fluxo automático

1. Seleciona/testa uma proxy fora dos países excluídos (`BR` por padrão), ou usa `--proxy`.
2. Inicia um relay HTTP local somente para `gateway.discord.gg`.
3. Abre o Discord com um PAC local: Gateway via relay; demais hosts em `DIRECT`.
4. O Gateway nasce através da proxy externa.
5. O launcher abre um Chrome DevTools Protocol (CDP) local em porta aleatória e localiza o renderer do Discord.
6. Injeta **somente um observador em memória**. Ele não altera experimentos/stores: observa eventos de voz e consulta `MediaEngineStore.supportsInApp(VIDEO/DESKTOP_CAPTURE)`.
7. Após a entrada em voz, quando `VIDEO` e `DESKTOP_CAPTURE` estiverem efetivamente liberados, o Go muda o relay para `DIRECT` para **novas conexões**.
8. O WebSocket Gateway que já nasceu pela proxy continua nessa rota até reconectar. O launcher pode encerrar; o relay fica em background enquanto o Discord estiver aberto.

Existe um fallback: ENTER força a mudança para `DIRECT` caso o hook deixe de reconhecer o Discord após alguma atualização.

## macOS Apple Silicon

```bash
chmod +x discord-golive-macos-arm64
./discord-golive-macos-arm64
```

## macOS Intel

```bash
chmod +x discord-golive-macos-amd64
./discord-golive-macos-amd64
```

## Windows x64

No PowerShell ou Prompt de Comando:

```powershell
.\discord-golive-windows-amd64.exe
```

## Proxy manual

```text
--proxy socks5://IP:PORTA
--proxy socks4://IP:PORTA
--proxy http://IP:PORTA
```

Exemplo:

```bash
./discord-golive-macos-arm64 --proxy socks5://127.0.0.1:9050
```

## Opções

```text
--protocol socks5|socks4|http
--exclude BR,XX
--proxy scheme://host:port
--no-reuse
--discord-bin CAMINHO
--direct
--help
```

`--direct` muda um relay já em execução para `DIRECT` em novas conexões sem fechar o Discord.

## Arquivos de estado

```text
~/.discord-golive/
```

O diretório contém logs, PIDs, modo do relay, porta de debugging e a última proxy funcional.

## Observações de segurança

- O CDP é habilitado apenas nesta inicialização do Discord, em uma porta aleatória. Ele permanece disponível durante essa instância do Discord; não exponha a porta nem execute software local não confiável durante a sessão.
- O observador injetado via CDP é temporário e desaparece ao fechar/recarregar o renderer. Não altera `app.asar` nem instala plugin.
- Proxies públicas podem ser instáveis ou mal administradas. O Gateway usa TLS, mas o operador da proxy observa metadados da conexão.
- Alterar rota/região para acessar funcionalidades pode contrariar regras do serviço. O comportamento pode mudar a qualquer momento.
- Os binários fornecidos são builds não assinados.
