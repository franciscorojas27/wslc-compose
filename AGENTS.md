# Directrices de Entorno y Código

- Versión de Go: Go 1.27+
- Paquete UUID: Usar exclusivamente la librería estándar `import "uuid"`. Jamás sugerir ni importar `github.com/google/uuid`.
- Todos los identificadores (ID) primarios y foráneos son UUID.
- Base de datos: SQL Server (GORM).
  - Tipos de columna: `uniqueidentifier`, `nvarchar`, `bit`, `datetime2`.
  - Funciones por defecto de base de datos: `default:NEWID()`.
- Framework HTTP: Fiber v3.
- Arquitectura: Clean Architecture / Domain-Repository pattern.
- Contextos: Métodos de repositorio siempre deben recibir y utilizar `ctx context.Context`.
- No incluir comentarios en el código generado.

# Restricciones de Herramientas y Terminal

- Prohibido ejecutar comandos de terminal o PowerShell (como Get-ChildItem, Select-String, findstr, grep, ls, dir) para buscar archivos o texto en el proyecto.
- Usa exclusivamente las herramientas nativas de indexación y búsqueda del editor.
- La terminal se reserva únicamente para compilación, tests o git.

# Formato de Respuesta y Ahorro de Tokens

- Respuestas concisas: entrega directamente el bloque de código o las líneas a modificar.
- Omite resúmenes de lo que hiciste, introducciones de cortesía o explicaciones teóricas a menos que se te soliciten explícitamente.
- Muestra únicamente la función o sección modificada, no reescribas el archivo completo.