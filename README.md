# xcryptossh

This is an evolution of golang.org/x/crypto/ssh to fix memory leaks and provide for graceful shutdown. It is not API backwards compatible.

We also want to explore adding Read and Write deadlines to ssh.Conn, so that they can act like net.Conn.
